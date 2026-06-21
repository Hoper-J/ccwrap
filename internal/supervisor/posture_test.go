package supervisor

import (
	"net/url"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
)

func mkPostureURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

func samplePostureResult(t *testing.T) *preflight.Result {
	t.Helper()
	alias, err := modelalias.New(map[string]string{"claude-x": "gpt-y"}, "profile", false)
	if err != nil {
		t.Fatalf("modelalias.New: %v", err)
	}
	return &preflight.Result{
		APIBaseURL:            mkPostureURL(t, "https://user:secret@gw.example.com/v1"),
		ActiveProfileName:     "gateway",
		ActiveProfileProvider: "acme",
		Egress:                model.EgressConfig{Mode: "direct", Summary: "direct"},
		ModelAlias:            alias,
	}
}

// TestNewResolved_StripsUserinfoAndDerivesDisplay pins the once-at-construct
// derivations: userinfo strip, ModelAliasMode, FailPolicy, owned forward map.
func TestNewResolved_StripsUserinfoAndDerivesDisplay(t *testing.T) {
	r := newResolved(samplePostureResult(t))
	if got := r.display.apiBaseURL; got != "https://gw.example.com/v1" {
		t.Errorf("apiBaseURL display = %q, want userinfo stripped", got)
	}
	if got := r.display.exactUpstreamHost; got != "gw.example.com" {
		t.Errorf("exactUpstreamHost = %q, want gw.example.com", got)
	}
	if got := r.display.exactUpstreamBase; got != "https://gw.example.com/v1" {
		t.Errorf("exactUpstreamBase = %q, want userinfo stripped", got)
	}
	if r.display.modelAliasMode != model.ModelAliasRewrite {
		t.Errorf("modelAliasMode = %v, want rewrite (alias is non-empty)", r.display.modelAliasMode)
	}
	if r.display.failPolicy != model.FailClosed {
		t.Errorf("failPolicy = %v, want FailClosed", r.display.failPolicy)
	}
	if r.display.modelAliasForward["claude-x"] != "gpt-y" {
		t.Errorf("modelAliasForward not copied: %v", r.display.modelAliasForward)
	}
}

// TestDeriveInto_NativeTLSDialWinsElseSeed pins the dial-wins-else-seed rule
// that replaces publishPosture's first-paint seed guard + sticky special-cases.
func TestDeriveInto_NativeTLSDialWinsElseSeed(t *testing.T) {
	r := newResolved(samplePostureResult(t))
	cases := []struct {
		name      string
		toggle    bool
		dial      dialState
		wantState string
	}{
		{"no-dial-toggle-on", true, dialState{}, initialNativeTLSPosture(true)},
		{"no-dial-toggle-off", false, dialState{}, initialNativeTLSPosture(false)},
		{"dial-active-wins", true, dialState{nativeTLS: "active"}, "active"},
		{"dial-blocked-wins", true, dialState{nativeTLS: "blocked: x"}, "blocked: x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := posture{r: r, l: live{nativeTLS: tc.toggle}}
			var s model.Session
			p.deriveInto(&s, tc.dial)
			if s.NativeTLS != tc.wantState {
				t.Errorf("NativeTLS = %q, want %q", s.NativeTLS, tc.wantState)
			}
		})
	}
}

// TestDeriveInto_TogglesAndDialMirror pins that the live toggles and the
// dial-state fields land on the projection.
func TestDeriveInto_TogglesAndDialMirror(t *testing.T) {
	r := newResolved(samplePostureResult(t))
	p := posture{r: r, l: live{captureBodies: true, captureTelemetry: true}}
	var s model.Session
	p.deriveInto(&s, dialState{nativeTLSFallbacks: 2, nativeTLSLoaded: true})
	if !s.CaptureBodies || !s.CaptureTelemetry {
		t.Errorf("capture toggles not mirrored: %+v", s)
	}
	if s.NativeTLSFallbacks != 2 || !s.NativeTLSLoaded {
		t.Errorf("dial fallbacks/loaded not mirrored: %+v", s)
	}
	if s.ExactUpstreamHost != "gw.example.com" || s.ActiveProfileName != "gateway" {
		t.Errorf("identity/display not mirrored: %+v", s)
	}
}

// TestPosture_ToggleAndResolveCommute proves the structural commutativity that
// makes the preserveLiveToggles flag unnecessary: withResolved touches only the
// resolved half, withCaptureBodies only the live half, in either order.
func TestPosture_ToggleAndResolveCommute(t *testing.T) {
	base := posture{r: newResolved(samplePostureResult(t)), l: live{}}
	pre2 := samplePostureResult(t)
	pre2.Egress = model.EgressConfig{Mode: "socks5", Summary: "socks5://h"}
	r2 := newResolved(pre2)

	switchThenToggle := base.withResolved(r2).withCaptureBodies(true)
	toggleThenSwitch := base.withCaptureBodies(true).withResolved(r2)

	if switchThenToggle.l != toggleThenSwitch.l {
		t.Errorf("live halves differ: %+v vs %+v", switchThenToggle.l, toggleThenSwitch.l)
	}
	if !switchThenToggle.l.captureBodies || !toggleThenSwitch.l.captureBodies {
		t.Errorf("captureBodies not set in both orders")
	}
	if switchThenToggle.r.egress.Mode != "socks5" || toggleThenSwitch.r.egress.Mode != "socks5" {
		t.Errorf("resolved egress not applied in both orders")
	}
}
