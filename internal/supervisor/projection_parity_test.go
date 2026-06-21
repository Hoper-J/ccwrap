package supervisor

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
)

// equivalentRequest builds the model.SessionRouteRequest that, fed to the
// test-fixture path resolvedFromRequest, should yield a projection identical to
// newResolved(pre) on the production path. It supplies the two fields the
// production path DERIVES rather than carries: ExactUpstreamHost/Base from the
// URL, and FailPolicy = FailClosed (the only value production ever uses). Every
// other field is carried straight across.
func equivalentRequest(pre *preflight.Result) model.SessionRouteRequest {
	apiStr, host, base := "", "", ""
	if pre.APIBaseURL != nil {
		apiStr = pre.APIBaseURL.String()
		host = pre.APIBaseURL.Hostname()
		base = pre.APIBaseURL.String()
	}
	return model.SessionRouteRequest{
		APIBaseURL:        apiStr,
		RouteClass:        pre.RouteClass,
		RouteSource:       pre.RouteSource,
		RouteConfigSource: pre.RouteConfigSource,
		AuthMode:          pre.AuthMode,
		AuthSource:        pre.AuthSource,
		AuthConfigSource:  pre.AuthConfigSource,
		AuthPolicy:        pre.AuthPolicy,
		AuthBootstrap:     pre.AuthBootstrap,
		AuthBootstrapKind: pre.AuthBootstrapKind,
		ExactUpstreamHost: host,
		ExactUpstreamBase: base,
		FailPolicy:        model.FailClosed,
		OverrideAuth:      pre.OverrideAuth,
		Egress:            pre.Egress,
		ModelAlias: model.ModelAliasConfig{
			Forward:                  pre.ModelAlias.Forward,
			Source:                   pre.ModelAlias.Source,
			Strict:                   pre.ModelAlias.Strict,
			ProviderModelPassthrough: pre.ModelAlias.ProviderModelPassthrough,
		},
		UpstreamHeaders:           pre.UpstreamHeaders.Headers,
		UpstreamHeaderSource:      pre.UpstreamHeaders.Source,
		UpstreamHeaderFingerprint: pre.UpstreamHeaders.Fingerprint,
		ActiveProfileName:         pre.ActiveProfileName,
		ActiveProfileProvider:     pre.ActiveProfileProvider,
		MissingAuthEnv:            pre.MissingAuthEnv,
	}
}

// TestProjection_NewResolvedMatchesResolvedFromRequest is the permanent
// replacement for the discarded cross-branch golden harness. It proves the
// production projection (newResolved → deriveInto) is byte-identical to the
// test-fixture projection (resolvedFromRequest → deriveInto) for equivalent
// inputs. Because the ~90 SetRoute-based tests validate the resolvedFromRequest
// projection, this lockstep transitively validates the production path —
// closing the D1 coverage illusion (a newResolved regression would surface here
// even though every fixture test would otherwise stay green). Compared via JSON
// so nil-vs-empty map/slice differences don't masquerade as divergence.
func TestProjection_NewResolvedMatchesResolvedFromRequest(t *testing.T) {
	aliasOn, err := modelalias.New(map[string]string{"claude-3-5-sonnet": "gpt-4o"}, "profile", false)
	if err != nil {
		t.Fatalf("modelalias.New: %v", err)
	}
	aliasPassthrough, err := modelalias.New(map[string]string{"claude-3-opus": "provider/big"}, "profile", true)
	if err != nil {
		t.Fatalf("modelalias.New: %v", err)
	}

	fixtures := []struct {
		name string
		pre  *preflight.Result
	}{
		{
			name: "third-party-userinfo-alias",
			pre: &preflight.Result{
				APIBaseURL:            mkPostureURL(t, "https://user:secret@gw.example.com/v1"),
				RouteClass:            model.RouteClass("third_party"),
				RouteSource:           model.RouteSource("profile"),
				RouteConfigSource:     "profiles.json",
				AuthMode:              model.AuthMode("api_key"),
				AuthSource:            model.AuthSource("anthropic_api_key"),
				AuthPolicy:            model.AuthPolicy("strict"),
				AuthBootstrap:         model.AuthBootstrap("injected"),
				AuthBootstrapKind:     model.AuthBootstrapKind("env"),
				ActiveProfileName:     "gateway",
				ActiveProfileProvider: "acme",
				Egress:                model.EgressConfig{Mode: "direct", Source: "fallback", Summary: "direct"},
				ModelAlias:            aliasOn,
			},
		},
		{
			name: "first-party-no-alias",
			pre: &preflight.Result{
				APIBaseURL:  mkPostureURL(t, "https://api.anthropic.com"),
				RouteClass:  model.RouteClass("first_party"),
				RouteSource: model.RouteSource("fallback"),
				AuthMode:    model.AuthMode("passthrough"),
				AuthSource:  model.AuthSource("none"),
				Egress:      model.EgressConfig{Mode: "direct", Source: "fallback", Summary: "direct"},
			},
		},
		{
			name: "passthrough-forces-strict-false",
			pre: &preflight.Result{
				APIBaseURL:        mkPostureURL(t, "https://gw3.example.com/v2"),
				RouteClass:        model.RouteClass("third_party"),
				AuthMode:          model.AuthMode("auth_token"),
				AuthSource:        model.AuthSource("anthropic_auth_token"),
				ActiveProfileName: "native",
				Egress:            model.EgressConfig{Mode: "direct", Source: "fallback", Summary: "direct"},
				ModelAlias:        aliasPassthrough,
			},
		},
		{
			name: "nil-api-base-url",
			pre: &preflight.Result{
				RouteClass: model.RouteClass("first_party"),
				Egress:     model.EgressConfig{Mode: "direct", Source: "fallback", Summary: "direct"},
			},
		},
	}

	ds := dialState{nativeTLS: "active", nativeTLSFallbacks: 3, nativeTLSLoaded: true}
	tog := live{captureBodies: true, captureTelemetry: true, nativeTLS: true}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			rB, err := resolvedFromRequest(equivalentRequest(f.pre))
			if err != nil {
				t.Fatalf("resolvedFromRequest: %v", err)
			}
			var sNew, sFixture model.Session
			(&posture{r: newResolved(f.pre), l: tog}).deriveInto(&sNew, ds)
			(&posture{r: rB, l: tog}).deriveInto(&sFixture, ds)

			jNew, _ := json.Marshal(sNew)
			jFixture, _ := json.Marshal(sFixture)
			if string(jNew) != string(jFixture) {
				t.Errorf("production projection diverged from fixture projection (lockstep broken):\n newResolved:        %s\n resolvedFromRequest: %s", jNew, jFixture)
			}
		})
	}
}

// findPlantedSecret reflect-walks v and returns the paths of any string field
// (incl. nested structs, maps, slices, pointers) whose value contains secret.
func findPlantedSecret(v reflect.Value, secret, path string, out *[]string) {
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), secret) {
			*out = append(*out, path)
		}
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			findPlantedSecret(v.Elem(), secret, path, out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			findPlantedSecret(v.Field(i), secret, path+"."+v.Type().Field(i).Name, out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			findPlantedSecret(v.MapIndex(k), secret, path+"["+k.String()+"]", out)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			findPlantedSecret(v.Index(i), secret, path+"[]", out)
		}
	}
}

// TestProjection_NoSecretLeaksIntoSession plants a known credential in every
// secret-bearing posture input (URL userinfo, the auth override header value,
// and the egress proxy URLs) and asserts the projected model.Session — the
// struct served at /recent / SSE — contains it NOWHERE. Today non-exposure is
// guaranteed by model.Session simply not having those fields; this reflect-walk
// locks that against a FUTURE field addition that accidentally surfaces a
// credential (and against a deriveInto that forgets to strip userinfo).
func TestProjection_NoSecretLeaksIntoSession(t *testing.T) {
	const secret = "PLANTED_SECRET_sk-ant-do-not-leak"
	proxyURL := "https://user:" + secret + "@proxy.internal:8080"
	alias, err := modelalias.New(map[string]string{"claude-x": "gpt-y"}, "profile", false)
	if err != nil {
		t.Fatalf("modelalias.New: %v", err)
	}
	pre := &preflight.Result{
		APIBaseURL:        mkPostureURL(t, "https://user:"+secret+"@gw.example.com/v1"),
		RouteClass:        model.RouteClass("third_party"),
		AuthMode:          model.AuthMode("api_key"),
		AuthSource:        model.AuthSource("anthropic_api_key"),
		ActiveProfileName: "gateway",
		OverrideAuth:      &model.AuthOverride{Mode: model.AuthMode("api_key"), Source: model.AuthSource("anthropic_api_key"), HeaderName: "x-api-key", HeaderValue: secret},
		Egress:            model.EgressConfig{Mode: "http_proxy", Source: "flag", Summary: proxyURL, HTTPProxy: proxyURL, HTTPSProxy: proxyURL},
		ModelAlias:        alias,
	}

	// Positive control: the walker MUST detect a secret planted in a projected
	// field — guards this test against silently becoming a no-op if
	// findPlantedSecret ever regresses.
	var control model.Session
	control.ActiveProfileName = secret
	var found []string
	findPlantedSecret(reflect.ValueOf(control), secret, "Session", &found)
	if len(found) == 0 {
		t.Fatal("positive control failed: findPlantedSecret did not detect a secret planted in a projected field")
	}

	var s model.Session
	(&posture{r: newResolved(pre), l: live{captureBodies: true, captureTelemetry: true, nativeTLS: true}}).deriveInto(&s, dialState{nativeTLS: "active"})

	var leaks []string
	findPlantedSecret(reflect.ValueOf(s), secret, "Session", &leaks)
	if len(leaks) > 0 {
		t.Errorf("credential leaked into the projected Session (served at /recent): %v\nfull session: %+v", leaks, s)
	}
}
