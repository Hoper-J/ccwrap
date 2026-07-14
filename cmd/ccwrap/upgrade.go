package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/update"
)

// applyTimeout caps the whole binary download+replace. The check itself
// stays on update.CheckTimeout; a release archive over a slow egress
// needs real headroom.
const applyTimeout = 10 * time.Minute

// allChannelsHint is the fallback manual command for when the channel
// is unknown. The npm package already went through the npm→ccwrap-cli
// rename; hardcoded commands scattered across call sites are a real
// drift risk, so they live in one place.
const allChannelsHint = "npm install -g ccwrap-cli@latest  |  curl -fsSL https://raw.githubusercontent.com/Hoper-J/ccwrap/main/install.sh | sh  |  go install github.com/Hoper-J/ccwrap/cmd/ccwrap@latest"

// upgradeRun / upgradeDetect / upgradeLookPath are test seams: channel
// dispatch is a table-tested pure function (update.DetectChannel), but
// the command glue still deserves coverage without executing real
// package managers — or requiring them in PATH.
var upgradeRun = runCommand

var upgradeDetect = detectRunningChannel

var upgradeLookPath = exec.LookPath

func runCommand(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func detectRunningChannel() (string, update.Channel, error) {
	exePath, err := update.ResolveExecutable()
	if err != nil {
		return "", update.ChannelSource, err
	}
	bi, _ := debug.ReadBuildInfo()
	return exePath, update.DetectChannel(exePath, versionBase, bi), nil
}

// syncUpdateCheck is the shared EXPLICIT check (upgrade, version
// --check): loud on failure, and it refreshes the cache so the next
// launch banner reads fresh facts. Explicit actions deliberately ignore
// CCWRAP_NO_UPDATE_CHECK — the user just asked.
func syncUpdateCheck(stateDir, egressFlag string) (string, error) {
	envMap := preflight.ParentEnvMap(os.Environ())
	cfg, warnings, err := egress.Resolve(egressFlag, envMap)
	if err != nil {
		return "", err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "ccwrap: "+w)
	}
	ctx, cancel := context.WithTimeout(context.Background(), update.CheckTimeout)
	defer cancel()
	latest, err := update.FetchLatest(ctx, update.NewClient(cfg, update.CheckTimeout), update.CheckURL(os.Getenv))
	if err != nil {
		return "", err
	}
	_ = update.SaveCache(stateDir, update.Cache{CheckedAt: time.Now().UTC(), Latest: latest})
	return latest, nil
}

func upgradeCommand(paths app.Paths, args []string) error {
	egressFlag := "auto"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--egress-proxy":
			if i+1 >= len(args) {
				return fmt.Errorf("--egress-proxy requires a value")
			}
			i++
			egressFlag = args[i]
		case strings.HasPrefix(arg, "--egress-proxy="):
			egressFlag = strings.TrimPrefix(arg, "--egress-proxy=")
		default:
			return fmt.Errorf("upgrade: unknown flag %s (supported: --egress-proxy)", arg)
		}
	}

	current := versionString()
	latest, err := syncUpdateCheck(paths.StateDir, egressFlag)
	if err != nil {
		// Failure-posture hard rule: every failure path must offer an
		// actionable manual command. The check fails before channel
		// detection ever runs — do one best-effort detection purely to
		// pick the right hint, and fall back to the all-channels hint
		// when detection fails too; network-flavored failures also
		// point at --egress-proxy.
		hint := allChannelsHint
		if _, ch, derr := upgradeDetect(); derr == nil {
			hint = update.ManualHint(ch)
		}
		return fmt.Errorf("upgrade: version check failed: %w\nmanual fallback:\n  %s\n(network issue? try --egress-proxy auto|direct|URL)", err, hint)
	}
	if !update.Newer(current, latest) {
		fmt.Printf("ccwrap %s is already the latest\n", current)
		return nil
	}

	exePath, ch, err := upgradeDetect()
	if err != nil {
		return fmt.Errorf("upgrade: cannot locate running binary: %w\nmanual fallback — reinstall via your original channel:\n  %s\nrelease: https://github.com/Hoper-J/ccwrap/releases/tag/v%s", err, allChannelsHint, latest)
	}

	switch ch {
	case update.ChannelSource:
		return fmt.Errorf("this is a source build — upgrade with: %s", update.ManualHint(ch))
	case update.ChannelBinary:
		envMap := preflight.ParentEnvMap(os.Environ())
		cfg, _, rerr := egress.Resolve(egressFlag, envMap)
		if rerr != nil {
			return rerr
		}
		ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
		defer cancel()
		// client timeout 0: the applyTimeout ctx caps the whole
		// download as one budget
		if aerr := update.Apply(ctx, update.NewClient(cfg, 0), update.DefaultReleaseBase, latest, runtime.GOOS, runtime.GOARCH, exePath, os.Stderr); aerr != nil {
			if errors.Is(aerr, os.ErrPermission) {
				return fmt.Errorf("%s is not writable — re-run as `sudo ccwrap upgrade`, or reinstall to a writable dir:\n  CCWRAP_BINDIR=~/.local/bin %s", filepath.Dir(exePath), update.ManualHint(ch))
			}
			return fmt.Errorf("upgrade failed: %w\nmanual fallback:\n  %s\nrelease: https://github.com/Hoper-J/ccwrap/releases/tag/v%s", aerr, update.ManualHint(ch), latest)
		}
	default:
		argv := update.UpgradeArgv(ch)
		if _, lerr := upgradeLookPath(argv[0]); lerr != nil {
			return fmt.Errorf("%s not found in PATH — run manually:\n  %s", argv[0], update.ManualHint(ch))
		}
		if rerr := upgradeRun(argv[0], argv[1:]...); rerr != nil {
			var exitErr *exec.ExitError
			if errors.As(rerr, &exitErr) {
				// The package manager's own output has already passed
				// through; the exit code passes through too — but first
				// add one line with ccwrap's conclusion, since that
				// output does not necessarily make the failure clear.
				fmt.Fprintf(os.Stderr, "ccwrap: %s exited with %d — upgrade did not complete\n", argv[0], exitErr.ExitCode())
				os.Exit(exitErr.ExitCode())
			}
			return fmt.Errorf("upgrade failed: %w\nmanual fallback:\n  %s", rerr, update.ManualHint(ch))
		}
	}

	fmt.Printf("ccwrap upgraded to %s\n", latest)
	fmt.Println("note: running sessions keep the old binary until relaunched")
	return nil
}
