package update

import (
	"testing"
	"time"
)

func TestClaudeCheckURL(t *testing.T) {
	for override, want := range map[string]string{
		"": DefaultClaudeCheckURL,
		"https://registry.npmmirror.com/ccwrap-cli/latest": "https://registry.npmmirror.com/@anthropic-ai/claude-code/latest",
		"https://mirror.corp/npm/ccwrap-cli/latest":        "https://mirror.corp/npm/@anthropic-ai/claude-code/latest",
		"https://updates.corp/ccwrap.json":                 "",
	} {
		getenv := func(k string) string {
			if k == "CCWRAP_UPDATE_CHECK_URL" {
				return override
			}
			return ""
		}
		if got := ClaudeCheckURL(getenv); got != want {
			t.Errorf("ClaudeCheckURL(%q) = %q, want %q", override, got, want)
		}
	}
}

func TestClaudeSeenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := LoadClaudeSeen(dir); ok {
		t.Fatal("no record yet must read as absent")
	}
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if err := SaveClaudeSeen(dir, "2.1.296", at); err != nil {
		t.Fatal(err)
	}
	if c, ok := LoadClaudeSeen(dir); !ok || c.Version != "2.1.296" || !c.SeenAt.Equal(at) {
		t.Fatalf("LoadClaudeSeen = %+v, %v", c, ok)
	}
}
