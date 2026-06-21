package supervisor

import "testing"

func TestIsTelemetryCaptureHost(t *testing.T) {
	on := []string{
		"http-intake.logs.us5.datadoghq.com",
		"anthropic.sentry.io",
	}
	off := []string{
		"api.anthropic.com",
		"mcp.datadoghq.com",                       // Datadog MCP server, NOT telemetry
		"console.statsig.com",                     // dashboard, NOT a data endpoint
		"mcp.sentry.dev",                          // Sentry MCP example
		"datadoghq.com",                           // bare apex must not match (exact-host only)
		"evil-http-intake.logs.us5.datadoghq.com", // no suffix tricks
	}
	for _, h := range on {
		if !isTelemetryCaptureHost(h) {
			t.Errorf("isTelemetryCaptureHost(%q) = false, want true", h)
		}
	}
	for _, h := range off {
		if isTelemetryCaptureHost(h) {
			t.Errorf("isTelemetryCaptureHost(%q) = true, want false", h)
		}
	}
}
