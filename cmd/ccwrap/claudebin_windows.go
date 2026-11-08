package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// npmShimExe finds the program in npm's batch shim for a native package:
// "%dp0%\node_modules\@anthropic-ai\claude-code\bin\claude.exe"   %*
var npmShimExe = regexp.MustCompile(`"%dp0%\\(node_modules\\[^"%]+\.exe)"`)

// directClaudeBin replaces a batch file with the executable it runs. On
// Windows `claude` on PATH is usually npm's claude.cmd, and running a batch
// file means cmd.exe parses the arguments a second time: the inline
// --settings JSON or a prompt with & % ^ " would not reach Claude Code as
// given. npm's shim for Claude Code only starts the package's claude.exe,
// so ccwrap starts that itself; any other batch file is refused.
func directClaudeBin(bin string) (string, error) {
	if ext := strings.ToLower(filepath.Ext(bin)); ext != ".cmd" && ext != ".bat" {
		return bin, nil
	}
	if data, err := os.ReadFile(bin); err == nil {
		if m := npmShimExe.FindSubmatch(data); m != nil {
			exe := filepath.Join(filepath.Dir(bin), string(m[1]))
			if fi, err := os.Stat(exe); err == nil && fi.Mode().IsRegular() {
				return exe, nil
			}
			return "", fmt.Errorf("%s starts %s, which is missing: Claude Code's install is incomplete, as an interrupted update leaves it\n"+
				"  reinstall Claude Code, or point ccwrap at a claude.exe with --claude-bin (or set CLAUDE_BIN)", bin, exe)
		}
	}
	return "", fmt.Errorf("%s is a batch file, and cmd.exe would reparse the arguments ccwrap passes to Claude Code\n"+
		"  point ccwrap at claude.exe with --claude-bin C:\\path\\to\\claude.exe (or set CLAUDE_BIN)", bin)
}
