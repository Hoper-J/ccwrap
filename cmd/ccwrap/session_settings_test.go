package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func testSessionSettingsDoc() map[string]any {
	return map[string]any{"env": map[string]any{
		"HTTPS_PROXY":         "http://127.0.0.1:41234",
		"NODE_EXTRA_CA_CERTS": "/state/ccwrap/certs/ca-cert.pem",
	}}
}

// Without a user settings FILE the doc goes inline: Claude Code then reports
// its own temp root as the flag-settings root, never a ccwrap path.
func TestSessionSettingsArgIsInlineJSONByDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CODE_TMPDIR", tmp)
	doc := testSessionSettingsDoc()

	arg, cleanup, err := sessionSettingsArg(doc, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(arg, "{") || !strings.HasSuffix(arg, "}") || strings.Contains(arg, "\n") {
		t.Fatalf("want single-line inline JSON (Claude Code's inline detection), got %q", arg)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(arg), &got); err != nil {
		t.Fatalf("inline arg is not JSON: %v", err)
	}
	if want := roundTrip(t, doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("inline doc = %v, want %v", got, want)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("inline mode must not write into Claude Code's temp root, found %v", entries)
	}
}

// A user settings FILE may hold secrets, so its merged doc must stay off argv:
// it goes into a 0600 file under Claude Code's per-uid temp root instead.
func TestSessionSettingsArgWithUserFileWritesUnderClaudeTempRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CODE_TMPDIR", tmp)
	doc := testSessionSettingsDoc()

	arg, cleanup, err := sessionSettingsArg(doc, true)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, fmt.Sprintf("claude-%d", os.Getuid()))
	if filepath.Dir(arg) != root {
		t.Fatalf("settings file %q not under Claude Code temp root %q", arg, root)
	}
	if !regexp.MustCompile(`^claude-settings-[0-9a-f]{16}\.json$`).MatchString(filepath.Base(arg)) {
		t.Fatalf("unexpected settings file name %q", filepath.Base(arg))
	}
	if strings.Contains(strings.ToLower(arg), "ccwrap") {
		t.Fatalf("settings path must not name ccwrap: %q", arg)
	}
	fi, err := os.Stat(arg)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("temp root mode = %v, want 0700", di.Mode().Perm())
	}
	data, err := os.ReadFile(arg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if want := roundTrip(t, doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("file doc = %v, want %v", got, want)
	}

	cleanup()
	if _, err := os.Stat(arg); !os.IsNotExist(err) {
		t.Fatalf("cleanup left %s behind (err=%v)", arg, err)
	}
}

// Claude Code creates CLAUDE_CODE_TMPDIR recursively; so must we.
func TestClaudeTempRootCreatesMissingBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "not", "yet")
	t.Setenv("CLAUDE_CODE_TMPDIR", base)
	arg, cleanup, err := sessionSettingsArg(testSessionSettingsDoc(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if want := filepath.Join(base, fmt.Sprintf("claude-%d", os.Getuid())); filepath.Dir(arg) != want {
		t.Fatalf("settings file %q not under %q", arg, want)
	}
}

// Like Claude Code, refuse a temp root that is a symlink (a pre-planted /tmp
// entry would otherwise receive the settings file).
func TestClaudeTempRootRefusesSymlink(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CODE_TMPDIR", tmp)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(tmp, fmt.Sprintf("claude-%d", os.Getuid()))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessionSettingsArg(testSessionSettingsDoc(), true); err == nil {
		t.Fatal("expected a symlinked Claude Code temp root to be refused")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("settings file written through the symlink: %v", entries)
	}
}

func roundTrip(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
