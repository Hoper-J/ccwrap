package supervisor

import "strings"

// telemetryCaptureHosts is the exact-host allowlist of Claude Code telemetry
// data endpoints ccwrap MITMs when telemetry capture is on. EXACT match only --
// vendors host non-telemetry services (MCP servers, dashboards) on the same
// apex (mcp.datadoghq.com, console.statsig.com) that must NOT be MITM'd.
// Distinct from web.go's isTelemetryHost, which is a broad suffix-match UI
// classifier for the activity "telemetry" chip; this gate decides interception.
// Grounded in claude-code 2.1.88 source + an empirical monitor run (2026-06-03);
// see the design spec. Statsig's data endpoint and user-configured OTEL are
// out of scope until observed.
var telemetryCaptureHosts = map[string]struct{}{
	"http-intake.logs.us5.datadoghq.com": {}, // Datadog logs (us5 region, hard-coded in CC)
	"anthropic.sentry.io":                {}, // Sentry error reporting (CC org ingest)
}

// isTelemetryCaptureHost reports whether host is on the exact-host telemetry
// capture allowlist (the MITM gate). Case-insensitive, trimmed, exact match.
func isTelemetryCaptureHost(host string) bool {
	_, ok := telemetryCaptureHosts[strings.ToLower(strings.TrimSpace(host))]
	return ok
}
