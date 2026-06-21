package profiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverwriteFile_CreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	f := &File{
		Default: "foo",
		Profiles: map[string]Profile{
			"foo": {Name: "foo", BaseURL: "https://api.example.com", Auth: nil},
		},
	}
	if err := OverwriteFile(path, f, "test"); err != nil {
		t.Fatalf("OverwriteFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load roundtrip: %v", err)
	}
	if got == nil || got.Default != "foo" {
		t.Fatalf("expected default=foo; got %+v", got)
	}
}

func TestOverwriteFile_ReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	fA := &File{Default: "a", Profiles: map[string]Profile{"a": {Name: "a", BaseURL: "https://a.example.com", Auth: nil}}}
	if err := OverwriteFile(path, fA, "test"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	fB := &File{Default: "b", Profiles: map[string]Profile{"b": {Name: "b", BaseURL: "https://b.example.com", Auth: nil}}}
	if err := OverwriteFile(path, fB, "test"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got File
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Default != "b" {
		t.Fatalf("expected default=b; got %q", got.Default)
	}
	if _, ok := got.Profiles["a"]; ok {
		t.Fatalf("expected profile a removed; got %+v", got.Profiles)
	}
}

func TestOverwriteFile_ValidateFails_NoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	// Pre-seed with valid file so we can verify it is untouched on failure.
	good := &File{Default: "good", Profiles: map[string]Profile{"good": {Name: "good", BaseURL: "https://api.example.com", Auth: nil}}}
	if err := OverwriteFile(path, good, "seed"); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	preBytes, _ := os.ReadFile(path)

	// Now attempt to overwrite with a *File that fails R3 (malformed BaseURL).
	bad := &File{Default: "bad", Profiles: map[string]Profile{"bad": {Name: "bad", BaseURL: ":::", Auth: nil}}}
	err := OverwriteFile(path, bad, "test")
	if err == nil {
		t.Fatalf("expected validation failure")
	}
	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("expected ErrValidationFailed; got %v", err)
	}
	// Verify pre-existing file untouched.
	postBytes, _ := os.ReadFile(path)
	if string(preBytes) != string(postBytes) {
		t.Fatalf("file mutated on validation failure")
	}
}

func TestOverwriteFile_NilFile_ReturnsPlainError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	err := OverwriteFile(path, nil, "test")
	if err == nil {
		t.Fatalf("expected error on nil file")
	}
	// A nil file is NOT a validation error.
	if errors.Is(err, ErrValidationFailed) {
		t.Fatalf("nil file should not be ErrValidationFailed; got %v", err)
	}
}

func TestOverwriteFile_SourceLabel_PropagatesToParseErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	bad := &File{Default: "bad", Profiles: map[string]Profile{"bad": {Name: "bad", BaseURL: "ftp://x", Auth: nil}}}
	err := OverwriteFile(path, bad, "profile add bad")
	if err == nil {
		t.Fatalf("expected error")
	}
	var perr *ParseErrors
	if !errors.As(err, &perr) {
		t.Fatalf("expected *ParseErrors; got %T", err)
	}
	if perr.Source != "profile add bad" {
		t.Fatalf("expected Source=%q; got %q", "profile add bad", perr.Source)
	}
}

func TestOverwriteFile_FileMode0600_OnCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	f := &File{Default: "a", Profiles: map[string]Profile{"a": {Name: "a", BaseURL: "https://a.example.com", Auth: nil}}}
	if err := OverwriteFile(path, f, "test"); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected file mode 0o600; got %o", got)
	}
}

func TestOverwriteFile_FileMode0600_NormalizesExisting0644(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	// Pre-seed at 0o644.
	if err := os.WriteFile(path, []byte(`{"default":"inherit-env","profiles":{}}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f := &File{Default: "a", Profiles: map[string]Profile{"a": {Name: "a", BaseURL: "https://a.example.com", Auth: nil}}}
	if err := OverwriteFile(path, f, "test"); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected file mode 0o600 after overwrite; got %o", got)
	}
}

func TestOverwriteFile_ParentDir0700_OnCreate(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "newstate")
	path := filepath.Join(subdir, "profiles.json")
	f := &File{Default: "a", Profiles: map[string]Profile{"a": {Name: "a", BaseURL: "https://a.example.com", Auth: nil}}}
	if err := OverwriteFile(path, f, "test"); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := os.Stat(subdir)
	if err != nil {
		t.Fatalf("stat subdir: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("expected dir mode 0o700; got %o", got)
	}
}

func TestOverwriteFile_TmpCleanupOnRenameFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	// Create a directory at the destination path so os.Rename fails with EISDIR.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	f := &File{Default: "a", Profiles: map[string]Profile{"a": {Name: "a", BaseURL: "https://a.example.com", Auth: nil}}}
	err := OverwriteFile(path, f, "test")
	if err == nil {
		t.Fatalf("expected rename failure")
	}
	// Scan for leftover .profiles.json.*.tmp files in dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".profiles.json.") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestOverwriteFile_TmpNameUnique_SerialCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	f := &File{Default: "a", Profiles: map[string]Profile{"a": {Name: "a", BaseURL: "https://a.example.com", Auth: nil}}}
	for i := 0; i < 5; i++ {
		if err := OverwriteFile(path, f, "test"); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	// Verify no leftover tmp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".profiles.json.") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover tmp file after serial calls: %s", e.Name())
		}
	}
}
