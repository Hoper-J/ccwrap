package supervisor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
)

// anthropicOAuthHosts is the exact-host set of Anthropic-owned non-
// .anthropic.com domains that participate in the Claude Code OAuth flow.
// Verified destinations from claude-code/src/constants/oauth.ts:
//   - platform.claude.com — OAuth authorize + token endpoint (refresh
//     POSTs land here: TOKEN_URL = https://platform.claude.com/v1/oauth/token)
//   - claude.com — OAuth redirect bounce (/cai/oauth/authorize)
//   - claude.ai — CLAUDE_AI_ORIGIN + CIMD client-metadata host
//
// Listed exact, not suffix-matched: a `.claude.com` suffix would also
// capture code.claude.com (docs) and docs.claude.com (general docs),
// widening MITM scope past auth-critical traffic for no observability
// benefit.
var anthropicOAuthHosts = map[string]bool{
	"platform.claude.com": true,
	"claude.com":          true,
	"claude.ai":           true,
}

// isAnthropicHost reports whether host should be MITM'd by ccwrap's Anthropic
// CA. True for: (a) any *.anthropic.com subdomain (suffix match — the
// historical scope: api, api-staging, mcp-proxy, telemetry, etc.) and
// (b) the exact-host OAuth domain set in anthropicOAuthHosts. The OAuth
// hosts are added so refresh-token POSTs are observable in inspect-web
// instead of falling through to handleBlindTunnel.
func isAnthropicHost(h string) bool {
	h = normalizeProxyHost(h)
	if strings.HasSuffix(h, ".anthropic.com") {
		return true
	}
	return anthropicOAuthHosts[h]
}

func isAnthropicAPIHost(h string) bool {
	h = normalizeProxyHost(h)
	return h == "api.anthropic.com" || h == "api-staging.anthropic.com"
}

func isThirdPartyRouteClass(class model.RouteClass) bool {
	return class == model.RouteClassThirdPartyHidden || class == model.RouteClassThirdPartyCompatible
}

func cleanAPIPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func isMessagesCreatePath(method, path string) bool {
	return strings.EqualFold(method, http.MethodPost) && cleanAPIPath(path) == "/v1/messages"
}

func isCountTokensPath(method, path string) bool {
	return strings.EqualFold(method, http.MethodPost) && cleanAPIPath(path) == "/v1/messages/count_tokens"
}

func isModelsListPath(method, path string) bool {
	if !strings.EqualFold(method, http.MethodGet) && !strings.EqualFold(method, http.MethodHead) {
		return false
	}
	return cleanAPIPath(path) == "/v1/models"
}

func isMessagesBatchPath(method, path string) bool {
	p := cleanAPIPath(path)
	if p == "/v1/messages/batches" {
		return strings.EqualFold(method, http.MethodGet) || strings.EqualFold(method, http.MethodPost)
	}
	if strings.HasPrefix(p, "/v1/messages/batches/") {
		return strings.EqualFold(method, http.MethodGet) || strings.EqualFold(method, http.MethodHead) || strings.EqualFold(method, http.MethodDelete)
	}
	return false
}

func isImplementedThirdPartyGatewayPath(method, path string, allowProviderModelPassthrough bool) bool {
	if isMessagesCreatePath(method, path) || isCountTokensPath(method, path) || isMessagesBatchPath(method, path) {
		return true
	}
	if isModelsListPath(method, path) {
		return allowProviderModelPassthrough
	}
	return false
}

func isSyntheticModelsPath(method, path string, allowProviderModelPassthrough bool) bool {
	return isModelsListPath(method, path) && !allowProviderModelPassthrough
}

type syntheticSpec struct {
	Status int
	Body   map[string]any
}

func firstPartySyntheticSpec(method, path string) (syntheticSpec, bool) {
	if !isSyntheticEligibleMethod(method) {
		return syntheticSpec{}, false
	}
	switch cleanAPIPath(path) {
	case "/api/claude_cli/bootstrap":
		return syntheticSpec{Status: http.StatusOK, Body: map[string]any{
			"client_data":              nil,
			"additional_model_options": []any{},
		}}, true
	case "/v1/mcp_servers":
		return syntheticSpec{Status: http.StatusOK, Body: map[string]any{
			"data":     []any{},
			"has_more": false,
		}}, true
	case "/mcp-registry/v0/servers":
		return syntheticSpec{Status: http.StatusOK, Body: map[string]any{
			"servers":  []any{},
			"has_more": false,
		}}, true
	case "/api/claude_code/user_settings", "/api/claude_code/team_memory":
		// Cross-device cloud-sync GETs. Claude Code's clients accept 200/404
		// (settingsSync validateStatus 200||404; teamMemorySync 200||304||404)
		// and treat 404 as "no data exists yet". They do NOT accept the default
		// synthetic 204, which makes their axios call throw. Synthesize a
		// bodyless 404 so the (fail-open, non-blocking) sync cleanly reads empty
		// instead of logging an error. ccwrap does not back these features; this
		// only removes a benign client-side throw.
		return syntheticSpec{Status: http.StatusNotFound}, true
	}
	return syntheticSpec{}, false
}

func isSyntheticEligibleMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func normalizeProxyHost(h string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(h), "."))
}

// handleThirdPartySyntheticOrBlock consults the request-captured ap for the
// routeClass + alias-config routing reads. The synthetic generators receive ap
// so they can stamp identity onto recordSyntheticRequest under the same
// captured posture: a synthetic-204 is recorded under the posture captured at
// handler entry even if a profile switch happens mid-handler. ap must be
// non-nil (handler entry guarantees this).
func (sp *sessionProxy) handleThirdPartySyntheticOrBlock(w http.ResponseWriter, r *http.Request, logicalHost string, start time.Time, ap *posture) bool {
	if !isAnthropicAPIHost(logicalHost) || !isThirdPartyRouteClass(ap.r.routeClass) {
		return false
	}
	aliasCfg := ap.r.modelAlias
	if isSyntheticModelsPath(r.Method, r.URL.Path, aliasCfg.ProviderModelPassthrough) {
		sp.writeSyntheticModels(w, r, start, logicalHost, aliasCfg, ap)
		return true
	}
	if isImplementedThirdPartyGatewayPath(r.Method, r.URL.Path, aliasCfg.ProviderModelPassthrough) {
		return false
	}
	if spec, ok := firstPartySyntheticSpec(r.Method, r.URL.Path); ok {
		sp.writeSyntheticShapedResponse(w, r, spec, start, logicalHost, ap)
		return true
	}
	sp.writeSyntheticDefault204(w, r, start, logicalHost, ap)
	return true
}

func (sp *sessionProxy) writeSyntheticModels(w http.ResponseWriter, r *http.Request, start time.Time, logicalHost string, aliasCfg modelalias.Config, ap *posture) {
	ids := make([]string, 0, len(aliasCfg.Forward))
	for logical := range aliasCfg.Forward {
		ids = append(ids, logical)
	}
	sort.Strings(ids)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "type": "model", "display_name": id})
	}
	payload := map[string]any{"object": "list", "data": data, "has_more": false}
	if len(ids) > 0 {
		payload["first_id"] = ids[0]
		payload["last_id"] = ids[len(ids)-1]
	}
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if strings.EqualFold(r.Method, http.MethodHead) {
		w.WriteHeader(status)
	} else {
		_ = json.NewEncoder(w).Encode(payload)
	}
	sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sp.session.public.ID,
		Category:  "route",
		Summary:   "served synthetic models list",
		Detail:    fmt.Sprintf("%s %s -> ccwrap-synthetic models=%d", logicalHost, cleanAPIPath(r.URL.Path), len(ids)),
	})
	sp.recordSyntheticRequest(start, r.Method, logicalHost, "ccwrap-synthetic", r.URL, status, model.StreamStateHTTP, ap)
}

func (sp *sessionProxy) writeSyntheticShapedResponse(w http.ResponseWriter, r *http.Request, spec syntheticSpec, start time.Time, logicalHost string, ap *posture) {
	status := spec.Status
	if status == 0 {
		status = http.StatusNoContent
	}
	if spec.Body != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	if strings.EqualFold(r.Method, http.MethodHead) || spec.Body == nil {
		w.WriteHeader(status)
	} else {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(spec.Body)
	}
	sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sp.session.public.ID,
		Category:  "route",
		Summary:   "served synthetic shaped response",
		Detail:    fmt.Sprintf("%s %s -> ccwrap-synthetic", logicalHost, cleanAPIPath(r.URL.Path)),
	})
	sp.recordSyntheticRequest(start, r.Method, logicalHost, "ccwrap-synthetic", r.URL, status, model.StreamStateHTTP, ap)
}

func (sp *sessionProxy) writeSyntheticDefault204(w http.ResponseWriter, r *http.Request, start time.Time, logicalHost string, ap *posture) {
	status := http.StatusNoContent
	w.WriteHeader(status)
	sp.supervisor.recordTrace(sp.session.public.ID, model.TraceRecord{
		Timestamp: time.Now(),
		SessionID: sp.session.public.ID,
		Category:  "route",
		Summary:   "served synthetic 204 default",
		Detail:    fmt.Sprintf("%s %s -> ccwrap-synthetic", logicalHost, cleanAPIPath(r.URL.Path)),
	})
	sp.recordSyntheticRequest(start, r.Method, logicalHost, "ccwrap-synthetic", r.URL, status, model.StreamStateHTTP, ap)
}

// recordSyntheticRequest stores a request record for a response that
// was synthesized locally (no upstream forward). The real HTTP method
// is preserved on Method so downstream consumers filtering by method
// see the truth; the Synthetic flag flags the synthesized origin so
// the UI can render it distinctly without relying on a string sentinel.
// The handler-captured ap is threaded in so that stamping
// ActiveProfileName/Provider onto RequestRecord uses the same per-request
// posture the synthetic response was generated under — no lazy re-read.
func (sp *sessionProxy) recordSyntheticRequest(start time.Time, method, logicalHost, actualHost string, u *url.URL, status int, stream model.StreamState, ap *posture) {
	rec := model.RequestRecord{
		Timestamp:          start,
		SessionID:          sp.session.public.ID,
		Method:             method,
		Synthetic:          true,
		LogicalTargetHost:  logicalHost,
		ActualUpstreamHost: actualHost,
		Path:               pathAndQuery(u),
		StatusCode:         status,
		LatencyMS:          time.Since(start).Milliseconds(),
		StreamState:        stream,
	}
	// Stamp identity from the captured *posture so a synthetic
	// response (blocked third-party / path-aware deny / etc.) records
	// under the posture it was generated under, not the latest posture.
	// ap is non-nil at all call sites in pathaware.go (each caller threads
	// its own ap.Load() through).
	if ap != nil {
		rec.ActiveProfileName = ap.r.profileName
		rec.ActiveProfileProvider = ap.r.profileProvider
	}
	sp.supervisor.recordRequest(sp.session.public.ID, rec)
}
