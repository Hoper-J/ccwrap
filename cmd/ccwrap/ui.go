package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/uiservice"
)

// uiCommand runs and manages `ccwrap ui`, the per-user app service.
// `serve` runs it in the foreground (ccwrap starts it detached on demand);
// `status` and `stop` manage a running one.
func uiCommand(paths app.Paths, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "serve":
		exe, err := ccwrapExecutable()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		srv := uiservice.New(uiservice.Options{
			Paths:   paths,
			Version: versionString(),
			Exe:     exe,
			Logger:  log.New(os.Stderr, "ccwrap ui: ", log.LstdFlags),
		})
		return srv.Serve(ctx)
	case "status", "":
		// Running ignores a runtime dir it can't trust; say why instead of
		// "not running".
		if err := app.CheckPrivateDir(paths.RuntimeDir); err != nil {
			return err
		}
		info, ok := uiservice.Running(context.Background(), paths)
		if !ok {
			fmt.Println("ccwrap ui is not running; `ccwrap open` starts it")
			return nil
		}
		fmt.Printf("ccwrap ui %s · pid %d · %s\n", info.Version, info.PID, info.URL())
		if info.PortNote != "" {
			fmt.Println("  " + info.PortNote)
		}
		return nil
	case "stop":
		stopped, err := uiservice.Stop(context.Background(), paths, 3*time.Second)
		if err != nil {
			return err
		}
		if stopped {
			fmt.Println("ccwrap ui stopped")
		} else {
			fmt.Println("ccwrap ui is not running")
		}
		return nil
	default:
		return fmt.Errorf("usage: ccwrap ui [status | stop | serve]")
	}
}

// ccwrapExecutable is this binary's resolved path, which sessions launched
// from the app run.
func ccwrapExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}
