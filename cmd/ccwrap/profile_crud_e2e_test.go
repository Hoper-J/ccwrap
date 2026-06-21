package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestProfileCRUD_E2E walks the full create/read/update/delete cycle,
// asserting at each step that subsequent reads reflect the change.
func TestProfileCRUD_E2E(t *testing.T) {
	paths := pathsFor(t)
	path := profiles.DefaultPath(paths.StateDir)

	// 1. add foo (becomes default via --set-default)
	out, errBuf := captureBoth(t, func() int {
		return profileAdd(paths, []string{
			"foo",
			"--base-url", "https://foo.example.com",
			"--auth-mode", "ccwrap_bearer", "--auth-key", "sk-foo",
			"--set-default",
		})
	})
	if out == "" {
		t.Fatalf("step 1 add: expected stdout; stderr=%q", errBuf)
	}

	// 2. ls — foo is active
	out, _ = captureBoth(t, func() int { return profileLs(paths, "") })
	if !strings.Contains(out, "* foo") {
		t.Fatalf("step 2 ls: expected '* foo'; got %q", out)
	}

	// 3. edit foo --provider acme
	if code := profileEdit(paths, []string{"foo", "--provider", "acme"}); code != 0 {
		t.Fatalf("step 3 edit: code=%d", code)
	}

	// 4. add bar via stdin secret
	stdin := strings.NewReader("sk-bar-secret\n")
	code := profileAddIO(paths, stdin, []string{
		"bar",
		"--base-url", "https://bar.example.com",
		"--auth-mode", "ccwrap_bearer",
		"--auth-key-stdin",
	})
	if code != 0 {
		t.Fatalf("step 4 add bar: code=%d", code)
	}

	// 5. set-default bar
	if code := profileSetDefault(paths, []string{"bar"}); code != 0 {
		t.Fatalf("step 5 set-default: code=%d", code)
	}

	// 6. ls — bar is active, foo present
	out, _ = captureBoth(t, func() int { return profileLs(paths, "") })
	if !strings.Contains(out, "* bar") {
		t.Fatalf("step 6 ls: expected '* bar'; got %q", out)
	}
	if !strings.Contains(out, "foo") {
		t.Fatalf("step 6 ls: expected 'foo' still present; got %q", out)
	}

	// 7. rm foo (not default — quiet success)
	if code := profileRm(paths, []string{"foo"}); code != 0 {
		t.Fatalf("step 7 rm foo: code=%d", code)
	}

	// 8. rm bar --force (was default — gets reset to inherit-env)
	if code := profileRm(paths, []string{"bar", "--force"}); code != 0 {
		t.Fatalf("step 8 rm bar: code=%d", code)
	}

	// 9. File still exists at 0o600, content is legal zero-state.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("step 9: file should exist; %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("step 9: expected 0o600; got %o", got)
	}
	file, _ := profiles.Load(path)
	if file == nil || file.Default != profiles.InheritEnv || len(file.Profiles) != 0 {
		t.Fatalf("step 9: expected zero-state; got %+v", file)
	}

	// 10. ls — emits "no profiles.json present (zero-touch / inherit-env)"
	out, _ = captureBoth(t, func() int { return profileLs(paths, "") })
	if !strings.Contains(out, "no profiles.json present") {
		t.Fatalf("step 10 ls: expected zero-state message; got %q", out)
	}
}

// captureBoth returns (stdout, stderr) from fn.
func captureBoth(t *testing.T, fn func() int) (string, string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	origOut := stdOut
	origErr := stdErr
	stdOut = func() io.Writer { return &outBuf }
	stdErr = func() io.Writer { return &errBuf }
	t.Cleanup(func() {
		stdOut = origOut
		stdErr = origErr
	})
	_ = fn()
	return outBuf.String(), errBuf.String()
}
