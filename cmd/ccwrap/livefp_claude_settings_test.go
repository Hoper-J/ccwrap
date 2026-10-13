//go:build livefp

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveClaudeSettingsRoute checks the two Claude Code behaviors
// preflight.LaunchBypass rests on, with routing env in Claude's user
// settings pointing at local sinks:
//
//  1. on its own credentials (an apiKeyHelper), Claude Code applies the
//     settings' ANTHROPIC_BASE_URL. If it stops doing so, the launch check only
//     refuses more than it must: logged as CLAUDE_SETTINGS_ROUTE_DRIFT (a
//     warning in the weekly workflow);
//  2. with the environment ccwrap's placeholder posture gives it (bearer
//     placeholder, host-managed flag), Claude Code drops both that base URL
//     and a Bedrock provider switch from its settings and goes to
//     api.anthropic.com. Reaching either sink here would mean third-party
//     routes leak past ccwrap: a hard failure.
//
// Nothing leaves the machine. claude comes from CCWRAP_LIVEFP_CLAUDE_BIN or
// PATH; without it the test skips, unless CCWRAP_LIVEFP_REQUIRE_CLAUDE=1.
//
//	go test ./cmd/ccwrap/ -tags livefp -run TestLiveClaudeSettingsRoute -v
func TestLiveClaudeSettingsRoute(t *testing.T) {
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
	root, err := os.MkdirTemp("", "livefp-settings-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	caFile := filepath.Join(root, "origin-ca.pem")
	api := startLiveHeadSink(t, true, caFile)
	proxy := startLiveConnectProxy(t, api.addr)
	netEnv := []string{
		"HTTPS_PROXY=http://" + proxy, "HTTP_PROXY=http://" + proxy, "NO_PROXY=127.0.0.1,localhost",
		"NODE_EXTRA_CA_CERTS=" + caFile,
	}
	const helperKey = "livefp-helper-key"
	// A home whose Claude user settings carry env and Claude Code's own
	// credential.
	settingsHome := func(name string, env map[string]string) string {
		t.Helper()
		home := mkLiveHome(t, root, name)
		doc, err := json.Marshal(map[string]any{"apiKeyHelper": "echo " + helperKey, "env": env})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), doc, 0o600); err != nil {
			t.Fatal(err)
		}
		return home
	}

	// 1. Claude Code on its own credentials.
	gateway := startLiveHeadSink(t, false, "")
	home := settingsHome("home-own", map[string]string{"ANTHROPIC_BASE_URL": "http://" + gateway.addr})
	runLiveChild(t, gateway, claudeBin, home, netEnv)
	if head, ok := gateway.messagesHead(); !ok || !strings.Contains(head, helperKey) {
		t.Logf("CLAUDE_SETTINGS_ROUTE_DRIFT Claude Code on its own credentials no longer sends its requests to the ANTHROPIC_BASE_URL in its settings; preflight.LaunchBypass may now refuse launches it need not (reached the settings URL: %t)", ok)
	} else {
		t.Log("Claude Code on its own credentials follows the ANTHROPIC_BASE_URL in its settings")
	}

	// 2. The environment of ccwrap's placeholder posture.
	gateway, bedrock := startLiveHeadSink(t, false, ""), startLiveHeadSink(t, false, "")
	home = settingsHome("home-placeholder", map[string]string{
		"ANTHROPIC_BASE_URL":            "http://" + gateway.addr,
		"CLAUDE_CODE_USE_BEDROCK":       "1",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH": "1",
		"AWS_REGION":                    "us-east-1",
		"ANTHROPIC_BEDROCK_BASE_URL":    "http://" + bedrock.addr,
	})
	runLiveChild(t, api, claudeBin, home, append(netEnv,
		"ANTHROPIC_AUTH_TOKEN=ccwrap-livefp-placeholder",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
	))
	for what, sink := range map[string]*liveHeadSink{"the settings' ANTHROPIC_BASE_URL": gateway, "the settings' Bedrock endpoint": bedrock} {
		sink.mu.Lock()
		heads := sink.heads
		sink.mu.Unlock()
		if len(heads) > 0 {
			t.Fatalf("with ccwrap's placeholder, Claude Code still sent a request to %s:\n%s", what, heads[0])
		}
	}
	if head := api.firstMessagesHead(t); !strings.Contains(head, "Bearer ccwrap-livefp-placeholder") {
		t.Fatalf("with ccwrap's placeholder, api.anthropic.com got no placeholder bearer:\n%s", head)
	}
	t.Log("with ccwrap's placeholder, Claude Code drops the routing env from its settings")
}
