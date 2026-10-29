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
