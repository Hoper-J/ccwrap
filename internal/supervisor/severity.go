package supervisor

import (
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// severityForClass maps an error_class to its (Severity, SessionHealth).
// Deliberate policy refusals and error statuses the upstream returned to a
// model request are "warn" (ccwrap itself worked); every transport /
// config failure — and any unknown/dynamic class — is "error". Single
// source of truth for both ErrorRecord.Severity and session Health.
func severityForClass(class string) (severity string, health model.SessionHealth) {
	switch {
	case class == "ccwrap_auth_missing", strings.HasPrefix(class, upstreamHTTPErrorClassPrefix):
		return "warn", model.HealthWarn
	default:
		return "error", model.HealthError
	}
}
