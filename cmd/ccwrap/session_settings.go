package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// sessionSettingsArg returns the value ccwrap passes to Claude Code's
// --settings flag, plus a cleanup for anything it wrote.
//
// Claude Code reports the directory of a --settings FILE upstream (auto
// mode's classifier context, safeguards[].classifier_context.rule_roots
// .flagSettings), so a file in ccwrap's runtime dir would put "ccwrap" and
// the session id into every request. For inline JSON it reports its own
// temp root instead — the same value as any `claude --settings '{…}'`
// launch — so inline is the default.
//
// Inline content is visible in the process list, though. When the user
// passed their own settings FILE (which may hold secrets) the merged doc
// goes into a 0600 file under Claude Code's temp root instead: still no
// ccwrap path upstream, and the contents stay off argv.
func sessionSettingsArg(doc map[string]any, userSettingsFile bool) (string, func(), error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return "", nil, err
	}
	if !userSettingsFile {
		return string(data), func() {}, nil
	}
	root, err := claudeTempRoot()
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(root, "claude-settings-"+hex.EncodeToString(sum[:])[:16]+".json")
	if err := writeSessionSettingsFile(path, doc); err != nil {
		return "", nil, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// claudeTempRoot returns Claude Code's per-uid temp root,
// ${CLAUDE_CODE_TMPDIR:-/tmp}/claude-<uid>, creating it 0700. Like Claude
// Code it refuses a root that is a symlink or owned by another user (a
// pre-planted /tmp entry).
func claudeTempRoot() (string, error) {
	base := os.Getenv("CLAUDE_CODE_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	dir := filepath.Join(base, fmt.Sprintf("claude-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("refusing Claude Code temp dir %s: not a directory (a symlink?); set CLAUDE_CODE_TMPDIR to a directory you own", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("refusing Claude Code temp dir %s: owned by uid %d; set CLAUDE_CODE_TMPDIR to a directory you own", dir, st.Uid)
	}
	return dir, nil
}
