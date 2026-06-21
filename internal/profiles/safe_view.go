package profiles

import (
	"net/url"
	"strings"
)

// SafeCatalogItem is the source-of-truth catalog-wire shape. Every
// field is non-secret by construction:
//   - BaseURLHost/EgressHost are url.Parse(...).Hostname() — userinfo-free
//   - Auth (when non-nil) carries Mode + HasInlineKey + HasKeyEnv boolean
//     flags; the env-var NAME and the secret bytes never enter this struct
//   - Auth=nil expresses "ccwrap does not own auth for this profile" — the
//     wire field is omitted (or "auth": null) instead of carrying empty mode
//   - UpstreamHeaderCount is a count; header values are never emitted
//
// control.SafeCatalogItem in internal/control mirrors this shape for the
// wire boundary (same JSON tags); the supervisor converts between them via
// a struct conversion. Keeping SafeCatalogItem in internal/profiles
// preserves the dependency direction (control depends on no package; the
// supervisor depends on both).
type SafeCatalogItem struct {
	Name                string            `json:"name"`
	Provider            string            `json:"provider"`
	BaseURLHost         string            `json:"base_url_host"`
	BaseURL             string            `json:"base_url,omitempty"` // full URL minus userinfo — populates popover edit form
	Auth                *SafeAuthSpec     `json:"auth,omitempty"`     // nil = ccwrap does not own auth
	ModelAliasCount     int               `json:"model_alias_count,omitempty"`
	ModelAliases        map[string]string `json:"model_aliases,omitempty"` // claude name → upstream model; populates popover edit form
	UpstreamHeaderCount int               `json:"upstream_header_count,omitempty"`
	EgressMode          string            `json:"egress_mode"`
	EgressHost          string            `json:"egress_host,omitempty"`
	EgressURL           string            `json:"egress_url,omitempty"` // full URL minus userinfo, only set when EgressMode=http
}

// SafeAuthSpec is the wire mirror of an AuthSpec without secret bytes.
// Mode is categorical (ccwrap_bearer / ccwrap_x_api_key — mode=passthrough is
// rejected at Validate time). HasInlineKey is true when Auth.Key carries a
// value; HasKeyEnv is true when Auth.KeyEnv names an env var. Both flags
// are bool — neither the secret nor the env NAME leaks.
type SafeAuthSpec struct {
	Mode         string `json:"mode"`
	HasInlineKey bool   `json:"has_inline_key,omitempty"`
	HasKeyEnv    bool   `json:"has_key_env,omitempty"`
}

// SafeView returns a non-secret catalog view of p suitable for emission on
// the browser-facing /profile/catalog endpoint. The function-local scope
// touches Auth.Key bytes only to set HasInlineKey; the bytes never escape.
//
// When p.Auth is nil (ccwrap does not own auth for this profile), SafeView
// leaves SafeCatalogItem.Auth nil — the wire emits "auth": null (or omits
// the field entirely via omitempty). The popover reads item.auth as a
// nullable nested object; absent means "ccwrap does not inject auth header".
func (p *Profile) SafeView() SafeCatalogItem {
	host := ""
	baseURL := ""
	if raw := strings.TrimSpace(p.BaseURL); raw != "" {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			host = u.Host // url.URL.Host omits userinfo by construction
			u.User = nil  // strip userinfo from the full URL too
			baseURL = u.String()
		}
	}
	egressHost := ""
	egressURL := ""
	mode := strings.TrimSpace(strings.ToLower(p.Egress.Mode))
	if mode == "" {
		mode = "inherit"
	}
	if mode == "http" {
		if raw := strings.TrimSpace(p.Egress.URL); raw != "" {
			if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
				egressHost = u.Host
				u.User = nil
				egressURL = u.String()
			}
		}
	}
	var auth *SafeAuthSpec
	if p.Auth != nil {
		auth = &SafeAuthSpec{
			Mode:         strings.TrimSpace(p.Auth.Mode),
			HasInlineKey: strings.TrimSpace(p.Auth.Key) != "",
			HasKeyEnv:    strings.TrimSpace(p.Auth.KeyEnv) != "",
		}
	}
	// Defensive shallow copy of ModelAliases so consumers can't mutate the
	// Profile's internal map. The values are model names — non-secret —
	// safe to expose on the wire for the popover edit panel to prefill.
	var aliases map[string]string
	if len(p.ModelAliases) > 0 {
		aliases = make(map[string]string, len(p.ModelAliases))
		for k, v := range p.ModelAliases {
			aliases[k] = v
		}
	}
	return SafeCatalogItem{
		Name:                p.Name,
		Provider:            p.Provider,
		BaseURLHost:         host,
		BaseURL:             baseURL,
		Auth:                auth,
		ModelAliasCount:     len(p.ModelAliases),
		ModelAliases:        aliases,
		UpstreamHeaderCount: len(p.UpstreamHeaders),
		EgressMode:          mode,
		EgressHost:          egressHost,
		EgressURL:           egressURL,
	}
}
