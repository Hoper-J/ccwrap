package upstreamheaders

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

const (
	EnvHeaders     = "CCWRAP_UPSTREAM_HEADERS"
	EnvHeadersJSON = "CCWRAP_UPSTREAM_HEADERS_JSON"
	EnvHeadersFile = "CCWRAP_UPSTREAM_HEADERS_FILE"
)

type Config struct {
	Headers     map[string]string
	Source      string
	Fingerprint string
}

type ResolveOptions struct {
	ExplicitPairs       []string
	ExplicitFile        string
	ExplicitFileContent []byte // when set, this content wins over reading from disk
	ParentEnv           []string
	EnvFileContent      []byte // when set, this content wins over reading from disk
	FlagSettings        map[string]any
}

func Resolve(opts ResolveOptions) (Config, []string, error) {
	merged := map[string]string{}
	var sources []string
	if opts.FlagSettings != nil {
		headers, err := ExtractFromFlagSettings(opts.FlagSettings)
		if err != nil {
			return Config{}, nil, err
		}
		if len(headers) > 0 {
			merge(merged, headers)
			sources = append(sources, "flagSettings:ccwrap.upstreamHeaders")
		}
	}
	env := envSliceToMap(opts.ParentEnv)
	for _, name := range []string{EnvHeaders, EnvHeadersJSON} {
		if raw := strings.TrimSpace(env[name]); raw != "" {
			headers, err := ParseJSON([]byte(raw), name)
			if err != nil {
				return Config{}, nil, err
			}
			merge(merged, headers)
			sources = append(sources, name)
		}
	}
	file := strings.TrimSpace(opts.ExplicitFile)
	fileSource := "explicit:upstream-headers-file"
	contentSnapshot := opts.ExplicitFileContent
	if file == "" {
		file = strings.TrimSpace(env[EnvHeadersFile])
		fileSource = EnvHeadersFile
		contentSnapshot = opts.EnvFileContent
	}
	if file != "" || len(contentSnapshot) > 0 {
		var headers map[string]string
		if len(contentSnapshot) > 0 {
			// Content takes precedence: when a snapshot is present, disk MUST NOT be read.
			parsed, err := ParseJSON(contentSnapshot, fileSource)
			if err != nil {
				return Config{}, nil, err
			}
			headers = parsed
		} else {
			loaded, err := LoadFile(file)
			if err != nil {
				return Config{}, nil, err
			}
			headers = loaded
		}
		merge(merged, headers)
		sources = append(sources, fileSource)
	}
	if len(opts.ExplicitPairs) > 0 {
		headers, err := ParsePairs(opts.ExplicitPairs)
		if err != nil {
			return Config{}, nil, err
		}
		merge(merged, headers)
		sources = append(sources, "explicit:upstream-header")
	}
	sources = dedupe(sources)
	cfg, err := New(merged, strings.Join(sources, ", "))
	if err != nil {
		return Config{}, nil, err
	}
	return cfg, sources, nil
}

func New(headers map[string]string, source string) (Config, error) {
	cfg := Config{Source: strings.TrimSpace(source)}
	if len(headers) == 0 {
		return cfg, nil
	}
	clean := map[string]string{}
	for name, value := range headers {
		cname, err := normalizeName(name)
		if err != nil {
			return Config{}, err
		}
		if strings.ContainsAny(value, "\r\n") {
			return Config{}, fmt.Errorf("upstream header %s contains a newline", cname)
		}
		clean[cname] = value
	}
	cfg.Headers = clean
	cfg.Fingerprint = Fingerprint(clean)
	return cfg, nil
}

func LoadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read upstream headers file %s: %w", path, err)
	}
	return ParseJSON(data, path)
}

func ParseJSON(data []byte, source string) (map[string]string, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse upstream headers %s: %w", source, err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("upstream headers %s must be a JSON object", source)
	}
	if nested, ok := obj["upstreamHeaders"]; ok {
		return coerceHeaderMap(nested, source+".upstreamHeaders")
	}
	if nested, ok := obj["upstream_headers"]; ok {
		return coerceHeaderMap(nested, source+".upstream_headers")
	}
	if nested, ok := obj["headers"]; ok {
		return coerceHeaderMap(nested, source+".headers")
	}
	if nested, ok := obj["ccwrap"]; ok {
		ccwrap, ok := nested.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.ccwrap must be an object", source)
		}
		if h, ok := ccwrap["upstreamHeaders"]; ok {
			return coerceHeaderMap(h, source+".ccwrap.upstreamHeaders")
		}
		if h, ok := ccwrap["upstream_headers"]; ok {
			return coerceHeaderMap(h, source+".ccwrap.upstream_headers")
		}
	}
	return coerceHeaderMap(obj, source)
}

func ExtractFromFlagSettings(settings map[string]any) (map[string]string, error) {
	if settings == nil {
		return nil, nil
	}
	raw, ok := settings["ccwrap"]
	if !ok {
		return nil, nil
	}
	ccwrap, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("settings ccwrap must be an object")
	}
	h, ok := ccwrap["upstreamHeaders"]
	if !ok {
		h, ok = ccwrap["upstream_headers"]
	}
	if !ok {
		return nil, nil
	}
	return coerceHeaderMap(h, "settings.ccwrap.upstreamHeaders")
}

func ParsePairs(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range pairs {
		left, right, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("upstream header %q must use Name=Value", pair)
		}
		name, err := normalizeName(left)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(right, "\r\n") {
			return nil, fmt.Errorf("upstream header %s contains a newline", name)
		}
		out[name] = right
	}
	return out, nil
}

func Apply(h http.Header, headers map[string]string) {
	for name, value := range headers {
		h.Set(name, value)
	}
}

func Fingerprint(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, k := range keys {
		_, _ = hash.Write([]byte(k))
		_, _ = hash.Write([]byte("\x00"))
		_, _ = hash.Write([]byte(headers[k]))
		_, _ = hash.Write([]byte("\x00"))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))[:16]
}

func coerceHeaderMap(raw any, source string) (map[string]string, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object mapping header name to value", source)
	}
	out := map[string]string{}
	for k, v := range obj {
		name, err := normalizeName(k)
		if err != nil {
			return nil, err
		}
		var value string
		switch x := v.(type) {
		case string:
			value = x
		case float64, bool:
			value = fmt.Sprint(x)
		default:
			return nil, fmt.Errorf("%s[%s] must be a string, number, or boolean", source, k)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("%s[%s] contains a newline", source, k)
		}
		out[name] = value
	}
	return out, nil
}

func normalizeName(name string) (string, error) {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return "", fmt.Errorf("upstream header name must be non-empty")
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-':
		default:
			return "", fmt.Errorf("upstream header name %q is invalid", raw)
		}
	}
	return http.CanonicalHeaderKey(raw), nil
}

func merge(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}

func envSliceToMap(env []string) map[string]string {
	out := map[string]string{}
	for _, pair := range env {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
