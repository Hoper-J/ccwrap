package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

type Client struct {
	http       *http.Client
	streamHTTP *http.Client
	base       string
	socket     string
}

func NewClient(socketPath string) *Client {
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		http:       &http.Client{Transport: tr, Timeout: 30 * time.Second},
		streamHTTP: &http.Client{Transport: tr},
		base:       "http://unix",
		socket:     socketPath,
	}
}

func (c *Client) SocketPath() string { return c.socket }

func (c *Client) Status(ctx context.Context) (*model.StatusResponse, error) {
	var out model.StatusResponse
	if err := c.getJSON(ctx, "/v1/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListSessions(ctx context.Context) ([]model.Session, error) {
	var out model.SessionsResponse
	if err := c.getJSON(ctx, "/v1/sessions", &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func (c *Client) GetSession(ctx context.Context, id string) (*model.Session, error) {
	var out model.Session
	if err := c.getJSON(ctx, "/v1/sessions/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Requests(ctx context.Context, sessionID string) ([]model.RequestRecord, error) {
	path := "/v1/requests"
	if sessionID != "" {
		path += "?session_id=" + url.QueryEscape(sessionID)
	}
	var out model.RequestsResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

func (c *Client) Errors(ctx context.Context, sessionID string) ([]model.ErrorRecord, error) {
	path := "/v1/errors"
	if sessionID != "" {
		path += "?session_id=" + url.QueryEscape(sessionID)
	}
	var out model.ErrorsResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Errors, nil
}

func (c *Client) Trace(ctx context.Context, sessionID string) ([]model.TraceRecord, error) {
	path := "/v1/trace"
	if sessionID != "" {
		path += "?session_id=" + url.QueryEscape(sessionID)
	}
	var out model.TraceResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Trace, nil
}

func (c *Client) CreateSession(ctx context.Context, req model.SessionCreateRequest) (*model.Session, error) {
	var out model.SessionCreateResponse
	if err := c.postJSON(ctx, "/v1/sessions", req, &out); err != nil {
		return nil, err
	}
	return &out.Session, nil
}

// SetRoute is retained as a test fixture for configuring a session route
// directly (the production launch + SwitchProfile paths publish a routing
// posture in-process from a *preflight.Result via supervisor.newResolved, with
// no SessionRouteRequest round-trip). Kept alongside the supervisor-side
// setRoute handler and model.SessionRouteRequest so the test suite can drive a
// session's posture from a literal request.
func (c *Client) SetRoute(ctx context.Context, id string, req model.SessionRouteRequest) error {
	// /route post-attach returns 409 with a typed
	// {"reason_code","message"} body. The typed-409 branch decodes the body
	// into a *RouteError when reason_code is present; anything else falls
	// through to the generic flat-error path.
	return c.postJSONWithTypedConflict(ctx, "/v1/sessions/"+url.PathEscape(id)+"/route", req, nil)
}

func (c *Client) Attach(ctx context.Context, id string, req model.SessionAttachRequest) error {
	return c.postJSON(ctx, "/v1/sessions/"+url.PathEscape(id)+"/attach", req, nil)
}

func (c *Client) CloseSession(ctx context.Context, id string, req model.SessionCloseRequest) error {
	return c.postJSON(ctx, "/v1/sessions/"+url.PathEscape(id)+"/close", req, nil)
}

func (c *Client) Shutdown(ctx context.Context) error {
	return c.postJSON(ctx, "/v1/shutdown", nil, nil)
}

// SwitchProfile calls the supervisor's profile-switch control op and decodes
// the structured outcome into a *SwitchOutcomeView.
//
// The endpoint is POST /v1/sessions/{id}/profile with JSON body
// {"name": "<requested>"}. The outcome is always structured (errors live
// INSIDE the outcome — never returned/logged raw across the boundary), so a
// non-2xx HTTP status does NOT mean SwitchProfile returned err; a
// transport-level failure (socket unreachable, JSON malformed) does. Callers
// inspect out.Result to decide what to render.
func (c *Client) SwitchProfile(ctx context.Context, sessionID, name string) (*SwitchOutcomeView, error) {
	body := map[string]string{"name": name}
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/sessions/"+url.PathEscape(sessionID)+"/profile", buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// The outcome is structured at 200 (the supervisor wraps all switch-side
	// errors INSIDE the outcome). A non-2xx surfaces here as a
	// transport-level error so callers can distinguish "switch produced an
	// outcome" from "the call itself failed to land".
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out SwitchOutcomeView
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode switch outcome: %w", err)
	}
	return &out, nil
}

func (c *Client) StreamEvents(ctx context.Context, handler func(model.Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/events", nil)
	if err != nil {
		return err
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("events failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	s := bufio.NewScanner(resp.Body)
	var data strings.Builder
	for s.Scan() {
		line := s.Text()
		if line == "" {
			if data.Len() > 0 {
				var ev model.Event
				if err := json.Unmarshal([]byte(data.String()), &ev); err == nil {
					handler(ev)
				}
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := s.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

func (c *Client) getJSON(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func (c *Client) postJSON(ctx context.Context, path string, in, out interface{}) error {
	buf := &bytes.Buffer{}
	if in != nil {
		if err := json.NewEncoder(buf).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, buf)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func decodeResponse(resp *http.Response, out interface{}) error {
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// postJSONWithTypedConflict is postJSON with a typed-409 branch.
// On 409 Conflict, the response body is peeked for the typed
// {"reason_code","message"} shape — if reason_code is non-empty, the call
// returns a *RouteError so callers can errors.As(err, &re). Any other status
// (including a 409 body that doesn't parse as the typed shape) falls through
// to decodeResponse's generic flat-error path, preserving existing behavior.
func (c *Client) postJSONWithTypedConflict(ctx context.Context, path string, in, out interface{}) error {
	buf := &bytes.Buffer{}
	if in != nil {
		if err := json.NewEncoder(buf).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, buf)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		// Buffer the body so we can attempt a typed decode AND, on failure,
		// hand the same bytes to the generic flat-error path. 8 KiB matches
		// decodeResponse's existing limit.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		var re RouteError
		if json.Unmarshal(raw, &re) == nil && re.Code != "" {
			return &re
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return decodeResponse(resp, out)
}
