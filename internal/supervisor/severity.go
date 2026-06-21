package supervisor

import "github.com/Hoper-J/ccwrap/internal/model"

// severityForClass maps an error_class to its (Severity, SessionHealth).
// Only deliberate policy refusals are "warn"; every genuine upstream /
// transport / config failure — and any unknown/dynamic class — is "error".
// Single source of truth for both ErrorRecord.Severity and session Health.
func severityForClass(class string) (severity string, health model.SessionHealth) {
	switch class {
	case "ccwrap_auth_missing":
		return "warn", model.HealthWarn
	default:
		return "error", model.HealthError
	}
}
