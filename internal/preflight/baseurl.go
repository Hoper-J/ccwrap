package preflight

import (
	"net/url"
	"strings"
)

// BaseURLProblem is what is wrong with a third-party base URL, told from the
// URL alone.
type BaseURLProblem struct {
	// OpenAI marks a provider's OpenAI-compatible endpoint, which Claude
	// Code can't use. Otherwise the base ends in /v1, and requests go to
	// /v1/v1/messages.
	OpenAI bool
	// Provider names the provider when ccwrap knows the endpoint.
	Provider string
	// Fixed is the base URL to use instead: the provider's
	// Anthropic-compatible one, or the base without its /v1. Empty for an
	// OpenAI-compatible endpoint of a provider ccwrap doesn't know.
	Fixed string
}

// CheckBaseURL reports a third-party base URL that can't work as it is.
func CheckBaseURL(base *url.URL) (BaseURLProblem, bool) {
	if base == nil {
		return BaseURLProblem{}, false
	}
	if p, ok := openAIBase(base); ok {
		return p, true
	}
	if fixed, ok := doubledV1Base(base); ok {
		return BaseURLProblem{Fixed: fixed}, true
	}
	return BaseURLProblem{}, false
}

// OpenAIEndpoint is a provider's OpenAI-compatible endpoint: its host, its
// paths ("" is the bare host; any other path matches itself and what is
// under it), and the provider's Anthropic-compatible base URL.
type OpenAIEndpoint struct {
	Host      string   `json:"host"`
	Paths     []string `json:"paths"`
	Provider  string   `json:"provider"`
	Anthropic string   `json:"anthropic"`
}

// openAIEndpoints are the OpenAI-compatible endpoints of providers that also
// serve Claude Code, which their docs and consoles show first.
var openAIEndpoints = []OpenAIEndpoint{
	{Host: "dashscope.aliyuncs.com", Paths: []string{"/compatible-mode"}, Provider: "Alibaba Cloud Model Studio", Anthropic: "https://dashscope.aliyuncs.com/apps/anthropic"},
	{Host: "ark.cn-beijing.volces.com", Paths: []string{"/api/v3"}, Provider: "Volcengine Ark", Anthropic: "https://ark.cn-beijing.volces.com/api/compatible"},
	{Host: "ark.cn-beijing.volces.com", Paths: []string{"/api/coding/v3"}, Provider: "Volcengine Ark Coding Plan", Anthropic: "https://ark.cn-beijing.volces.com/api/coding"},
	{Host: "open.bigmodel.cn", Paths: []string{"/api/paas/v4", "/api/coding/paas/v4"}, Provider: "Zhipu", Anthropic: "https://open.bigmodel.cn/api/anthropic"},
	{Host: "api.z.ai", Paths: []string{"/api/paas/v4", "/api/coding/paas/v4"}, Provider: "Z.ai", Anthropic: "https://api.z.ai/api/anthropic"},
	{Host: "api.deepseek.com", Paths: []string{"", "/v1"}, Provider: "DeepSeek", Anthropic: "https://api.deepseek.com/anthropic"},
	{Host: "api.moonshot.cn", Paths: []string{"/v1"}, Provider: "Moonshot", Anthropic: "https://api.moonshot.cn/anthropic"},
	{Host: "api.moonshot.ai", Paths: []string{"/v1"}, Provider: "Moonshot", Anthropic: "https://api.moonshot.ai/anthropic"},
	{Host: "api.siliconflow.cn", Paths: []string{"/v1"}, Provider: "SiliconFlow", Anthropic: "https://api.siliconflow.cn"},
}

// OpenAIEndpoints returns the known OpenAI-compatible endpoints, for the
// profile form to warn as the user types.
func OpenAIEndpoints() []OpenAIEndpoint {
	return append([]OpenAIEndpoint(nil), openAIEndpoints...)
}

// openAIBase reports an OpenAI-compatible endpoint: a known one, or a path
// that gives it away (…/chat/completions, a compatible-mode segment).
func openAIBase(base *url.URL) (BaseURLProblem, bool) {
	host := strings.ToLower(base.Hostname())
	path := strings.TrimRight(base.Path, "/")
	for _, e := range openAIEndpoints {
		if e.Host != host {
			continue
		}
		for _, p := range e.Paths {
			if path == p || (p != "" && strings.HasPrefix(path, p+"/")) {
				return BaseURLProblem{OpenAI: true, Provider: e.Provider, Fixed: e.Anthropic}, true
			}
		}
	}
	if strings.HasSuffix(path, "/chat/completions") || strings.Contains(path+"/", "/compatible-mode/") {
		return BaseURLProblem{OpenAI: true}, true
	}
	return BaseURLProblem{}, false
}

// doubledV1Base reports a base URL ending in /v1. Claude Code appends
// /v1/messages to its base URL, so such a base sends requests to
// /v1/v1/messages (gateway docs often quote the OpenAI-style /v1 form).
// fixed is the base without the /v1 and without userinfo.
func doubledV1Base(base *url.URL) (fixed string, ok bool) {
	p := strings.TrimRight(base.Path, "/")
	if !strings.HasSuffix(p, "/v1") {
		return "", false
	}
	u := *base
	u.User = nil
	u.Path = strings.TrimSuffix(p, "/v1")
	u.RawPath = ""
	return u.String(), true
}
