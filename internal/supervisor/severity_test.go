package supervisor

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestSeverityForClass(t *testing.T) {
	cases := []struct {
		class        string
		wantSeverity string
		wantHealth   model.SessionHealth
	}{
		{"ccwrap_auth_missing", "warn", model.HealthWarn},
		{"upstream_unreachable", "error", model.HealthError},
		{"tls_mitm_failed", "error", model.HealthError},
		{"model_alias_rewrite_failed", "error", model.HealthError},
		{"blind_tunnel_dial_failed", "error", model.HealthError},
		{"blind_tunnel_hijack_failed", "error", model.HealthError},
		{"blind_tunnel_handshake_failed", "error", model.HealthError},
		{"route_resolve_failed", "error", model.HealthError},
		{"anything_unknown", "error", model.HealthError},
	}
	for _, c := range cases {
		gotSev, gotHealth := severityForClass(c.class)
		if gotSev != c.wantSeverity || gotHealth != c.wantHealth {
			t.Errorf("class %q: got (%q,%q), want (%q,%q)",
				c.class, gotSev, gotHealth, c.wantSeverity, c.wantHealth)
		}
	}
}
