package supervisor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// The posture sentence under the hero follows live updates: the session on
// the wire carries the same text the first paint renders, and the page
// applies it.
func TestLiveHeroSentenceUsesServerText(t *testing.T) {
	sess := model.Session{RouteClass: model.RouteClassThirdPartyHidden, ModelAliasCount: 2, RecentErrorCount: 3, APIBaseURL: "https://gw.example"}
	wire := sessionWithDisplay(sess)
	if wire.Display.HeroSentence == "" || wire.Display.HeroSentence != ui.SessionPosture(&sess) {
		t.Fatalf("hero sentence = %q", wire.Display.HeroSentence)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	js := liftScriptFns(t, renderedInlineScript(t), "updateHeroBody")
	driver := `
var body = {textContent: 'stale'};
var document = {getElementById: function(id){ return id === 'hero-body' ? body : null; }};
var state = {session: ` + string(data) + `};
` + js + `
updateHeroBody();
process.stdout.write(JSON.stringify(body.textContent));`
	want, _ := json.Marshal(wire.Display.HeroSentence)
	if out := runNode(t, driver); string(out) != string(want) {
		t.Fatalf("live hero = %s, want %s", out, want)
	}
}

// An error changes the session's issue count and health, so it is followed
// by a session update carrying them.
func TestRecordErrorBroadcastsSessionUpdate(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "upstream_unreachable", Summary: "boom"})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != "session_updated" {
				continue
			}
			if s, ok := ev.Data.(model.Session); !ok || s.RecentErrorCount != 1 {
				t.Fatalf("session_updated data = %+v", ev.Data)
			}
			return
		case <-deadline:
			t.Fatal("no session_updated after recordError")
		}
	}
}
