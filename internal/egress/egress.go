package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func Resolve(flagValue string, env map[string]string) (model.EgressConfig, []string, error) {
	flagValue = strings.TrimSpace(flagValue)
	switch strings.ToLower(flagValue) {
	case "", "auto":
		cfg, notes, err := resolveFromEnv(env)
		return cfg, notes, err
	case "none", "direct":
		return model.EgressConfig{Mode: "direct", Source: "explicit_flag", Summary: "direct"}, nil, nil
	default:
		u, err := parseProxyURL(flagValue)
		if err != nil {
			return model.EgressConfig{}, nil, err
		}
		return model.EgressConfig{Mode: "explicit", HTTPSProxy: u.String(), HTTPProxy: u.String(), Source: "explicit_flag", Summary: redact(u)}, nil, nil
	}
}

// ProxyFunc returns an http.Transport-compatible proxy function.
// For SOCKS5 proxies, returns nil — use DialContextFunc instead.
func ProxyFunc(cfg model.EgressConfig) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		u := proxyURLForRequest(cfg, req.URL)
		if isSOCKS(u) {
			return nil, nil // SOCKS5 handled by DialContextFunc
		}
		return u, nil
	}
}

// IsSOCKSEgress reports whether the egress config uses a SOCKS5 proxy.
func IsSOCKSEgress(cfg model.EgressConfig) bool {
	u := selectProxy(cfg, "https")
	return isSOCKS(u)
}

// DialContextFunc returns a dialer that routes through the egress proxy.
// Use this on http.Transport.DialContext when the egress proxy is SOCKS5
// (since http.Transport.Proxy only supports HTTP/HTTPS proxies).
func DialContextFunc(cfg model.EgressConfig) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return DialContext(ctx, cfg, network, addr)
	}
}

func DialContext(ctx context.Context, cfg model.EgressConfig, network, address string) (net.Conn, error) {
	if network == "" {
		network = "tcp"
	}
	if shouldBypass(address, cfg.NoProxy) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}
	proxyURL := selectProxy(cfg, "https")
	if proxyURL == nil {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}
	if isSOCKS(proxyURL) {
		return dialSOCKS5Proxy(ctx, proxyURL, address)
	}
	return dialHTTPProxy(ctx, proxyURL, address)
}

func Summary(cfg model.EgressConfig) string {
	if strings.TrimSpace(cfg.Summary) != "" {
		return cfg.Summary
	}
	if cfg.Mode == "direct" || (cfg.HTTPProxy == "" && cfg.HTTPSProxy == "") {
		return "direct"
	}
	if cfg.HTTPSProxy != "" {
		if u, err := url.Parse(cfg.HTTPSProxy); err == nil {
			return redact(u)
		}
		return cfg.HTTPSProxy
	}
	if cfg.HTTPProxy != "" {
		if u, err := url.Parse(cfg.HTTPProxy); err == nil {
			return redact(u)
		}
		return cfg.HTTPProxy
	}
	return "direct"
}

func resolveFromEnv(env map[string]string) (model.EgressConfig, []string, error) {
	var notes []string
	cfg := model.EgressConfig{Mode: "auto", Source: "inherited_env"}
	cfg.NoProxy = first(env["no_proxy"], env["NO_PROXY"])
	if raw := first(env["https_proxy"], env["HTTPS_PROXY"]); raw != "" {
		u, err := parseProxyURL(raw)
		if err != nil {
			return model.EgressConfig{}, nil, fmt.Errorf("invalid HTTPS_PROXY: %w", err)
		}
		cfg.HTTPSProxy = u.String()
		cfg.Summary = redact(u)
	}
	if raw := first(env["http_proxy"], env["HTTP_PROXY"]); raw != "" {
		u, err := parseProxyURL(raw)
		if err != nil {
			return model.EgressConfig{}, nil, fmt.Errorf("invalid HTTP_PROXY: %w", err)
		}
		cfg.HTTPProxy = u.String()
		if cfg.Summary == "" {
			cfg.Summary = redact(u)
		}
	}
	if cfg.HTTPProxy == "" && cfg.HTTPSProxy == "" {
		if raw := first(env["all_proxy"], env["ALL_PROXY"]); raw != "" {
			u, err := parseProxyURL(raw)
			if err == nil {
				cfg.HTTPProxy = u.String()
				cfg.HTTPSProxy = u.String()
				cfg.Summary = redact(u)
			} else {
				notes = append(notes, "ALL_PROXY ignored: "+err.Error())
			}
		}
	}
	if cfg.HTTPProxy == "" && cfg.HTTPSProxy == "" {
		cfg.Mode = "direct"
		cfg.Source = "none"
		cfg.Summary = "direct"
	}
	return cfg, notes, nil
}

func proxyURLForRequest(cfg model.EgressConfig, reqURL *url.URL) *url.URL {
	if reqURL == nil || cfg.Mode == "direct" {
		return nil
	}
	target := targetHostPortForRequest(reqURL)
	if shouldBypass(target, cfg.NoProxy) {
		return nil
	}
	return selectProxy(cfg, reqURL.Scheme)
}

func targetHostPortForRequest(reqURL *url.URL) string {
	if reqURL == nil {
		return ""
	}
	target := reqURL.Host
	if target == "" {
		target = reqURL.Hostname()
	}
	host, port := normalizeHostPort(target)
	if host == "" {
		return target
	}
	if port == "" {
		switch strings.ToLower(strings.TrimSpace(reqURL.Scheme)) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	if port == "" {
		return target
	}
	return net.JoinHostPort(host, port)
}

func selectProxy(cfg model.EgressConfig, scheme string) *url.URL {
	var raw string
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http":
		raw = first(cfg.HTTPProxy, cfg.HTTPSProxy)
	default:
		raw = first(cfg.HTTPSProxy, cfg.HTTPProxy)
	}
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil
	}
	return u
}

func dialHTTPProxy(ctx context.Context, proxyURL *url.URL, address string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if deadline, ok := ctx.Deadline(); ok {
			_ = tlsConn.SetDeadline(deadline)
		} else {
			_ = tlsConn.SetDeadline(time.Now().Add(20 * time.Second))
		}
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = tlsConn.SetDeadline(time.Time{})
		conn = tlsConn
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	}
	bw := bufio.NewWriter(conn)
	fmt.Fprintf(bw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: ccwrap\r\n", address, address)
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		fmt.Fprintf(bw, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	fmt.Fprint(bw, "\r\n")
	if err := bw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

func dialSOCKS5Proxy(ctx context.Context, proxyURL *url.URL, address string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	}

	// SOCKS5 greeting: version 5
	hasAuth := proxyURL.User != nil
	if hasAuth {
		// offer username/password auth (0x02)
		_, err = conn.Write([]byte{0x05, 0x02, 0x00, 0x02})
	} else {
		// offer no-auth only (0x00)
		_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 greeting: %w", err)
	}

	// Read server choice
	var choice [2]byte
	if _, err := io.ReadFull(conn, choice[:]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 greeting response: %w", err)
	}
	if choice[0] != 0x05 {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: unexpected version %d", choice[0])
	}

	switch choice[1] {
	case 0x00:
		// no auth required
	case 0x02:
		// username/password auth (RFC 1929)
		if !hasAuth {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5: server requires auth but no credentials provided")
		}
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		if len(user) > 255 || len(pass) > 255 {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5: username or password too long")
		}
		authReq := []byte{0x01, byte(len(user))}
		authReq = append(authReq, []byte(user)...)
		authReq = append(authReq, byte(len(pass)))
		authReq = append(authReq, []byte(pass)...)
		if _, err := conn.Write(authReq); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5 auth: %w", err)
		}
		var authResp [2]byte
		if _, err := io.ReadFull(conn, authResp[:]); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5 auth response: %w", err)
		}
		if authResp[0] != 0x01 {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5: unexpected auth version %d", authResp[0])
		}
		if authResp[1] != 0x00 {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5: authentication failed")
		}
	case 0xFF:
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: no acceptable auth method")
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: unsupported auth method %d", choice[1])
	}

	// CONNECT request
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: invalid address %q: %w", address, err)
	}
	portNum, err := net.LookupPort("tcp", port)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: invalid port %q: %w", port, err)
	}

	connectReq := []byte{0x05, 0x01, 0x00} // version, CONNECT, reserved
	// ATYP selection: use IP types for IP literals, domain for hostnames.
	// socks5:// resolves DNS locally; socks5h:// sends domain for proxy-side resolution.
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			connectReq = append(connectReq, 0x01) // IPv4
			connectReq = append(connectReq, ip4...)
		} else if ip6 := ip.To16(); ip6 != nil {
			connectReq = append(connectReq, 0x04) // IPv6
			connectReq = append(connectReq, ip6...)
		}
	} else if proxyURL.Scheme == "socks5" {
		// socks5:// — resolve DNS locally, send IP
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5: resolve %q locally: %w", host, err)
		}
		if len(addrs) == 0 {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5: resolve %q locally: no addresses found", host)
		}
		ips := make([]net.IP, len(addrs))
		for i, a := range addrs {
			ips[i] = a.IP
		}
		if ip4 := ips[0].To4(); ip4 != nil {
			connectReq = append(connectReq, 0x01)
			connectReq = append(connectReq, ip4...)
		} else {
			connectReq = append(connectReq, 0x04)
			connectReq = append(connectReq, ips[0].To16()...)
		}
	} else {
		// socks5h:// — send domain name for proxy-side resolution
		if len(host) > 255 {
			_ = conn.Close()
			return nil, fmt.Errorf("socks5: FQDN too long (%d bytes)", len(host))
		}
		connectReq = append(connectReq, 0x03, byte(len(host)))
		connectReq = append(connectReq, []byte(host)...)
	}
	connectReq = append(connectReq, byte(portNum>>8), byte(portNum))

	if _, err := conn.Write(connectReq); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 connect: %w", err)
	}

	// Read CONNECT response header (VER, REP, RSV, ATYP)
	var respHead [4]byte
	if _, err := io.ReadFull(conn, respHead[:]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5 connect response: %w", err)
	}
	if respHead[0] != 0x05 {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: unexpected version in response %d", respHead[0])
	}
	if respHead[1] != 0x00 {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: connect failed: %s", socks5ReplyString(respHead[1]))
	}
	// Consume the bind address so the connection is ready for data.
	if err := consumeSOCKS5Addr(conn, respHead[3]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socks5: read bind address: %w", err)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// consumeSOCKS5Addr reads and discards a SOCKS5 address+port from conn
// based on the address type byte.
func consumeSOCKS5Addr(conn net.Conn, atyp byte) error {
	switch atyp {
	case 0x01: // IPv4: 4 bytes + 2 port
		var skip [4 + 2]byte
		_, err := io.ReadFull(conn, skip[:])
		return err
	case 0x04: // IPv6: 16 bytes + 2 port
		var skip [16 + 2]byte
		_, err := io.ReadFull(conn, skip[:])
		return err
	case 0x03: // Domain: 1 length + domain + 2 port
		var dlen [1]byte
		if _, err := io.ReadFull(conn, dlen[:]); err != nil {
			return err
		}
		skip := make([]byte, int(dlen[0])+2)
		_, err := io.ReadFull(conn, skip)
		return err
	default:
		return fmt.Errorf("unknown ATYP %d", atyp)
	}
}

func socks5ReplyString(code byte) string {
	switch code {
	case 0x00:
		return "succeeded"
	case 0x01:
		return "general SOCKS server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown error (0x%02x)", code)
	}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.r != nil && c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

func parseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy URL must include scheme and host")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	return u, nil
}

func isSOCKS(u *url.URL) bool {
	return u != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h")
}

func redact(u *url.URL) string {
	if u == nil {
		return "direct"
	}
	copy := *u
	if copy.User != nil {
		copy.User = url.UserPassword(copy.User.Username(), "***")
	}
	return copy.String()
}

// defaultBypassFloor is the always-on NO_PROXY floor: loopback, IPv4
// link-local (incl. the cloud-IMDS address 169.254.169.254), and the RFC1918
// private ranges are never sent through the egress proxy, regardless of the
// NO_PROXY env. This matches the default behavior of curl, Go's net/http, and
// Python requests, and mirrors Claude Code's hardcoded NO_PROXY_LIST floor
// (src/upstreamproxy/upstreamproxy.ts). It closes a footgun: without it, a
// user who sets HTTPS_PROXY but not NO_PROXY would route loopback and cloud
// metadata-service traffic through whatever the proxy is. User NO_PROXY
// entries are ADDITIVE on top of this floor — the floor is a minimum, never
// a replacement.
var defaultBypassFloor = []string{
	"localhost",
	"127.0.0.0/8",    // IPv4 loopback (entire RFC-reserved /8)
	"::1",            // IPv6 loopback
	"169.254.0.0/16", // IPv4 link-local, incl. cloud IMDS 169.254.169.254
	"10.0.0.0/8",     // RFC1918 private
	"172.16.0.0/12",  // RFC1918 private
	"192.168.0.0/16", // RFC1918 private
}

func shouldBypass(target, noProxy string) bool {
	host, port := normalizeHostPort(target)
	if host == "" {
		return false
	}
	// The floor applies unconditionally; user NO_PROXY entries are additive.
	if bypassMatchAny(host, port, defaultBypassFloor) {
		return true
	}
	return bypassMatchAny(host, port, splitNoProxy(noProxy))
}

// bypassMatchAny reports whether host[:port] matches any bypass token. A token
// is one of: "*" (match everything), a CIDR range (matched when host parses as
// an IP inside the range), a "host:port" pair (exact host AND port), a
// ".suffix" (apex + subdomain suffix match), or a bare host (exact match).
func bypassMatchAny(host, port string, tokens []string) bool {
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if token == "*" {
			return true
		}
		// CIDR token: match when the target host is an IP inside the range.
		// net.ParseCIDR fails for every non-CIDR token (hostnames, bare IPs,
		// host:port pairs, .suffix entries), so this branch never shadows the
		// host matchers below. Also fixes user-supplied CIDR NO_PROXY entries,
		// which were previously inert (treated as literal hostnames).
		if _, ipNet, err := net.ParseCIDR(token); err == nil {
			if ip := net.ParseIP(host); ip != nil && ipNet.Contains(ip) {
				return true
			}
			continue
		}
		tokenHost, tokenPort := normalizeHostPort(token)
		if tokenHost == "" {
			continue
		}
		// Claude Code treats host:port tokens as exact host+port matches. It
		// does not apply suffix matching to port-qualified NO_PROXY entries.
		if tokenPort != "" {
			if port != "" && host == tokenHost && port == tokenPort {
				return true
			}
			continue
		}
		if strings.HasPrefix(tokenHost, ".") {
			trimmed := strings.TrimPrefix(tokenHost, ".")
			if host == trimmed || strings.HasSuffix(host, tokenHost) {
				return true
			}
			continue
		}
		if host == tokenHost {
			return true
		}
	}
	return false
}

func normalizeHostPort(value string) (host, port string) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return "", ""
	}
	if h, p, err := net.SplitHostPort(trimmed); err == nil {
		return strings.Trim(strings.TrimSpace(h), "[]"), strings.TrimSpace(p)
	}
	if strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, "]") {
		end := strings.Index(trimmed, "]")
		return strings.Trim(trimmed[1:end], "[]"), strings.TrimPrefix(trimmed[end+1:], ":")
	}
	return strings.Trim(trimmed, "[]"), ""
}

func splitNoProxy(noProxy string) []string {
	fields := strings.FieldsFunc(noProxy, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		out = append(out, strings.ToLower(field))
	}
	return out
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
