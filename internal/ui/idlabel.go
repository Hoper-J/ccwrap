package ui

import "github.com/Hoper-J/ccwrap/internal/model"

// ShortID truncates a session ID to the 8-char form used by every
// surface's default rendering. The full ID surfaces only in
// --verbose / JSON / the web details drawer.
func ShortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// ClaudeSessionHeader is the request header Claude Code stamps on every
// /v1/messages call with the conversation's session UUID (the value that
// names the transcript under ~/.claude/projects; what --continue/--resume
// operate on). Single-sourced HERE for every surface — the web bootstrap,
// the supervisor scan, and the TUI header — so the spelling/casing can
// never drift between them.
const ClaudeSessionHeader = "X-Claude-Code-Session-Id"

// LatestClaudeSessionID returns the ClaudeSessionHeader value carried by
// the most recent record that has it. Records are appended oldest-first,
// so the scan runs from the end. Empty when no record carries the header
// (before the first /v1/messages, or telemetry/CONNECT-only traffic).
func LatestClaudeSessionID(records []model.RequestRecord) string {
	for i := len(records) - 1; i >= 0; i-- {
		if v := records[i].RequestHeaders.Get(ClaudeSessionHeader); v != "" {
			return v
		}
	}
	return ""
}

// ShortMethodLabel is the CLI/TUI synthetic label form: a dim "SYNTH"
// + the real method, distinct from Web's full "SYNTHETIC" (which keeps
// using model.RequestRecord.MethodLabel). The model package stays
// presentation-free; this lives in ui.
func ShortMethodLabel(rec model.RequestRecord) string {
	if rec.Synthetic {
		if rec.Method == "" {
			return "SYNTH"
		}
		return "SYNTH " + rec.Method
	}
	if rec.Method == "" {
		return "REQUEST"
	}
	return rec.Method
}
