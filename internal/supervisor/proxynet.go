package supervisor

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func isForwardProxyRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	return r.URL.IsAbs() || strings.TrimSpace(r.URL.Host) != ""
}
func detectStreamState(req *http.Request, respHeader http.Header) model.StreamState {
	if respHeader == nil {
		return model.StreamStateUnknown
	}
	if req.Header.Get("Upgrade") != "" || strings.EqualFold(respHeader.Get("Upgrade"), "websocket") {
		return model.StreamStateWebSocket
	}
	if strings.Contains(strings.ToLower(respHeader.Get("Content-Type")), "text/event-stream") {
		return model.StreamStateSSE
	}
	if strings.Contains(strings.ToLower(req.Header.Get("Content-Type")), "multipart/form-data") {
		return model.StreamStateMultipart
	}
	return model.StreamStateHTTP
}

func splitHostPort(hostport string) (string, string) {
	if strings.Contains(hostport, ":") {
		h, p, err := net.SplitHostPort(hostport)
		if err == nil {
			if p == "" {
				p = "443"
			}
			return h, p
		}
		if strings.Count(hostport, ":") == 1 {
			parts := strings.SplitN(hostport, ":", 2)
			if parts[0] != "" {
				return parts[0], parts[1]
			}
		}
	}
	return hostport, "443"
}

func (sp *sessionProxy) proxyCopy(dst net.Conn, src net.Conn) {
	// Outermost defer (runs last): the blind-tunnel copy loops are bare
	// goroutines spawned post-Hijack, OUTSIDE net/http's per-request panic
	// recovery. Since the supervisor shares the launcher process, an
	// unrecovered panic here would crash ccwrap and orphan Claude — so a
	// panic in io.Copy or either Close is logged, not fatal.
	defer recoverGoroutine("sessionProxy.proxyCopy")
	defer sp.wg.Done()
	defer dst.Close()
	defer src.Close()
	_, _ = io.Copy(dst, src)
}

func writeConnectEstablished(conn net.Conn) error {
	if conn == nil {
		return fmt.Errorf("nil connection")
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func redactedForwardProxyURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host := u.Host
	if host == "" {
		host = u.Hostname()
	}
	if host == "" {
		return scheme + "://<unknown>/..."
	}
	return scheme + "://" + host + "/..."
}

type singleConnListener struct {
	conn      net.Conn
	once      sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{conn: conn, closed: make(chan struct{})}
}

func (l *singleConnListener) signalClosed() {
	if l == nil {
		return
	}
	l.closeOnce.Do(func() { close(l.closed) })
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	var ok bool
	l.once.Do(func() {
		conn = &notifyCloseConn{Conn: l.conn, onClose: func() { l.signalClosed() }}
		ok = true
	})
	if ok {
		return conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	l.signalClosed()
	if l.conn != nil {
		return l.conn.Close()
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return dummyAddr("single-conn") }

type notifyCloseConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

type trackedNetConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *notifyCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

func (c *trackedNetConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

type dummyAddr string

func (d dummyAddr) Network() string { return string(d) }
func (d dummyAddr) String() string  { return string(d) }

type captureResponseWriter struct {
	http.ResponseWriter
	status      int
	header      http.Header
	wroteHeader bool
}

func newCaptureResponseWriter(w http.ResponseWriter) *captureResponseWriter {
	return &captureResponseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *captureResponseWriter) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *captureResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
	w.wroteHeader = true
	w.header = cloneHeader(w.ResponseWriter.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *captureResponseWriter) StatusCode() int { return w.status }

func (w *captureResponseWriter) HeaderSnapshot() http.Header {
	if w.header == nil {
		return cloneHeader(w.ResponseWriter.Header())
	}
	return cloneHeader(w.header)
}

func (w *captureResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijacker unsupported")
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, values := range h {
		out[k] = append([]string(nil), values...)
	}
	return out
}
