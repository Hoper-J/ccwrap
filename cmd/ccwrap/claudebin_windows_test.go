package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const npmClaudeShim = "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\n" +
	`"%dp0%\node_modules\@anthropic-ai\claude-code\bin\claude.exe"   %*` + "\r\n"

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDirectClaudeBinRunsTheExeBehindNpmsShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "claude.cmd")
	writeFile(t, shim, npmClaudeShim)
	exe := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	writeFile(t, exe, "MZ")
	got, err := directClaudeBin(shim)
	if err != nil || got != exe {
		t.Fatalf("directClaudeBin(npm shim) = %q, %v; want %q", got, err, exe)
	}
}

func TestDirectClaudeBinRefusesOtherBatchFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		// npm's shim for a JavaScript package runs node.exe, not Claude Code.
		"js-shim.cmd": "IF EXIST \"%dp0%\\node.exe\" (\r\n  SET \"_prog=%dp0%\\node.exe\"\r\n)\r\n\"%_prog%\" \"%dp0%\\node_modules\\@anthropic-ai\\claude-code\\cli.js\" %*\r\n",
		"wrapper.bat": "@echo off\r\nclaude.exe %*\r\n",
		// The shim's target is missing.
		"dangling.cmd": npmClaudeShim,
	} {
		path := filepath.Join(dir, name)
		writeFile(t, path, content)
		got, err := directClaudeBin(path)
		if err == nil || !strings.Contains(err.Error(), "--claude-bin") {
			t.Errorf("directClaudeBin(%s) = %q, %v; want a refusal naming --claude-bin", name, got, err)
		}
		if name == "dangling.cmd" && (err == nil || !strings.Contains(err.Error(), "is missing")) {
			t.Errorf("directClaudeBin(dangling shim) = %v; want it to say the program is missing", err)
		}
	}
}

func TestDirectClaudeBinKeepsExecutables(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "claude.exe")
	if got, err := directClaudeBin(exe); err != nil || got != exe {
		t.Fatalf("directClaudeBin(exe) = %q, %v; want it unchanged", got, err)
	}
}
