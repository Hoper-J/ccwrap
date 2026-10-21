package ui

import (
	"fmt"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// RequestModelLabel renders a request's model for activity rows: the model
// Claude Code asked for, the upstream model when an alias changed it, and a
// marker when a Claude model went out with no alias. Kept in sync with
// requestModelLabel in the web template.
func RequestModelLabel(rec model.RequestRecord) string {
	switch {
	case rec.Model == "":
		return ""
	case rec.UpstreamModel != "" && rec.UpstreamModel != rec.Model:
		return rec.Model + " → " + rec.UpstreamModel
	case rec.ModelUnmapped:
		return rec.Model + " (no alias)"
	default:
		return rec.Model
	}
}

// RequestFailureHead is the one-line heading for a failed request, e.g.
// "upstream returned 404 · not_found_error". Kept in sync with
// failureHead in the web template.
func RequestFailureHead(rec model.RequestRecord) string {
	f := rec.Failure
	if f == nil {
		return ""
	}
	if f.Source == "ccwrap" {
		return "no upstream response"
	}
	head := fmt.Sprintf("upstream returned %d", rec.StatusCode)
	if f.Type != "" {
		head += " · " + f.Type
	}
	return head
}

// RequestFailureBrief is the failure message cut to n runes, for the
// one-line request rows.
func RequestFailureBrief(rec model.RequestRecord, n int) string {
	if rec.Failure == nil || rec.Failure.Message == "" {
		return ""
	}
	r := []rune(rec.Failure.Message)
	if len(r) <= n {
		return rec.Failure.Message
	}
	return string(r[:n]) + "…"
}

// DisplayPath is a request path for activity rows: Claude Code tags every
// API call with ?beta=true, which says nothing per row, so that parameter is
// dropped; any other query is kept. Kept in sync with displayPath in the
// web template.
func DisplayPath(path string) string {
	base, query, ok := strings.Cut(path, "?")
	if !ok {
		return path
	}
	kept := make([]string, 0, 2)
	for _, p := range strings.Split(query, "&") {
		if p != "" && p != "beta=true" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}

// ErrorSeverityLabel names an error-list entry's kind for activity rows:
// "warning" for warn severity (e.g. an error status the upstream returned),
// "error" otherwise. Kept in sync with the web template's error rows.
func ErrorSeverityLabel(rec model.ErrorRecord) string {
	if rec.Severity == "warn" {
		return "warning"
	}
	return "error"
}
