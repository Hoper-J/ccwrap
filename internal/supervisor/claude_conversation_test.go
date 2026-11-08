package supervisor

import (
	"net/http"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// The session keeps the Claude Code conversation its latest model request
// belongs to, so a page can resume exactly that conversation once the
// session has ended.
func TestSessionKeepsClaudeConversation(t *testing.T) {
	srv, sess := newTestSessionForCreate(t)
	id := sess.snapshot().ID
	send := func(conversation string) {
		h := http.Header{}
		if conversation != "" {
			h.Set(ui.ClaudeSessionHeader, conversation)
		}
		srv.recordRequest(id, model.RequestRecord{SessionID: id, Method: http.MethodPost, Path: "/v1/messages", RequestHeaders: h})
	}
	send("")
	if got := sess.snapshot().ClaudeSessionID; got != "" {
		t.Fatalf("before any conversation: %q", got)
	}
	first, second := "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
	send(first)
	send("") // requests without the header leave it as it was
	if got := sess.snapshot().ClaudeSessionID; got != first {
		t.Fatalf("after the first conversation: %q", got)
	}
	send(second)
	if got := sess.snapshot().ClaudeSessionID; got != second {
		t.Fatalf("after a later conversation: %q", got)
	}
}

// The session keeps the Claude Code version its requests name; other
// clients' requests leave it as it was.
func TestSessionKeepsClaudeVersion(t *testing.T) {
	srv, sess := newTestSessionForCreate(t)
	id := sess.snapshot().ID
	send := func(ua string) {
		h := http.Header{}
		h.Set("User-Agent", ua)
		srv.recordRequest(id, model.RequestRecord{SessionID: id, Method: http.MethodPost, Path: "/v1/messages", RequestHeaders: h})
	}
	send("claude-cli/2.1.296 (external, sdk-cli)")
	send("curl/8.7.1")
	if got := sess.snapshot().ClaudeVersion; got != "2.1.296" {
		t.Fatalf("ClaudeVersion = %q, want 2.1.296", got)
	}
}
