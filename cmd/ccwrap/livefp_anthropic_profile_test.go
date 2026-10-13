//go:build livefp

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/preflight"
)

// TestLiveClaudeAnthropicProfile checks the two Claude Code behaviors
// preflight.LaunchBypass rests on, with an implicit Anthropic SDK
// profile (~/.config/anthropic/configs/default.json) whose base_url is a local
// sink:
//
//  1. on its own credentials, Claude Code signs in with that profile and sends
//     its requests to the base_url. If it stops doing so, the launch check
//     only refuses more than it must: logged as CLAUDE_ANTHROPIC_PROFILE_DRIFT
//     (a warning in the weekly workflow);
//  2. with the environment ccwrap's placeholder posture gives it (bearer
//     placeholder, host-managed flag), Claude Code leaves the profile alone and
//     goes to api.anthropic.com. Reaching the profile's sink here would mean
//     third-party routes leak past ccwrap: a hard failure.
//
// Nothing leaves the machine. claude comes from CCWRAP_LIVEFP_CLAUDE_BIN or
// PATH; without it the test skips, unless CCWRAP_LIVEFP_REQUIRE_CLAUDE=1.
//
//	go test ./cmd/ccwrap/ -tags livefp -run TestLiveClaudeAnthropicProfile -v
func TestLiveClaudeAnthropicProfile(t *testing.T) {
	claudeBin := os.Getenv("CCWRAP_LIVEFP_CLAUDE_BIN")
	if claudeBin == "" {
		claudeBin, _ = exec.LookPath("claude")
	}
	if claudeBin == "" {
		if os.Getenv("CCWRAP_LIVEFP_REQUIRE_CLAUDE") != "" {
			t.Fatal("claude not found and CCWRAP_LIVEFP_REQUIRE_CLAUDE is set")
		}
		t.Skip("claude not found; put it on PATH or set CCWRAP_LIVEFP_CLAUDE_BIN")
	}
	root, err := os.MkdirTemp("", "livefp-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	caFile := filepath.Join(root, "origin-ca.pem")
	api := startLiveHeadSink(t, true, caFile)
	proxy := startLiveConnectProxy(t, api.addr)
	const profileToken = "livefp-profile-token"
	// A home whose implicit profile points at its own plain-HTTP sink.
	profileHome := func(name string) (string, *liveHeadSink) {
		t.Helper()
		sink := startLiveHeadSink(t, false, "")
		home := mkLiveHome(t, root, name)
		store := filepath.Join(home, ".config", "anthropic")
		for path, data := range map[string]string{
			filepath.Join(store, "configs", "default.json"):     `{"authentication":{"type":"user_oauth"},"base_url":"http://` + sink.addr + `"}`,
			filepath.Join(store, "credentials", "default.json"): `{"access_token":"` + profileToken + `"}`,
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if p := preflight.ActiveAnthropicProfile(map[string]string{"HOME": home}, home); !p.LeavesAnthropicAPI() {
			t.Fatalf("ccwrap's mirror does not pick the fixture profile: %+v", p)
		}
		return home, sink
	}
	netEnv := []string{
		"HTTPS_PROXY=http://" + proxy, "HTTP_PROXY=http://" + proxy, "NO_PROXY=127.0.0.1,localhost",
		"NODE_EXTRA_CA_CERTS=" + caFile,
	}

	// 1. Claude Code on its own credentials.
	home, sink := profileHome("home-own")
	runLiveChild(t, sink, claudeBin, home, netEnv)
	if head, ok := sink.messagesHead(); !ok || !strings.Contains(head, "Bearer "+profileToken) {
		t.Logf("CLAUDE_ANTHROPIC_PROFILE_DRIFT Claude Code no longer sends its requests to an implicit Anthropic profile's base_url with the profile's token; preflight.LaunchBypass may now refuse launches it need not (reached the profile sink: %t)", ok)
	} else {
		t.Log("Claude Code signs in with the implicit profile and sends to its base_url")
	}

	// 2. The environment of ccwrap's placeholder posture.
	home, sink = profileHome("home-placeholder")
	runLiveChild(t, api, claudeBin, home, append(netEnv,
		"ANTHROPIC_AUTH_TOKEN=ccwrap-livefp-placeholder",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
	))
	if head, ok := sink.messagesHead(); ok {
		t.Fatalf("with ccwrap's placeholder, Claude Code still sent its request to the Anthropic profile's base_url:\n%s", head)
	}
	if head := api.firstMessagesHead(t); !strings.Contains(head, "Bearer ccwrap-livefp-placeholder") {
		t.Fatalf("with ccwrap's placeholder, api.anthropic.com got no placeholder bearer:\n%s", head)
	}
	t.Log("with ccwrap's placeholder, Claude Code leaves the profile alone")
}
