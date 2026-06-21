package supervisor

import (
	"bytes"
	"net"
	"os"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"

	utls "github.com/refraction-networking/utls"
)

func TestValidateLoadedHello(t *testing.T) {
	undici, err := os.ReadFile("testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLoadedHello(undici); err != nil {
		t.Errorf("undici hello (http/1.1 ALPN) must validate, got %v", err)
	}

	// An h2-offering hello must be rejected.
	uc := utls.UClient(nil, &utls.Config{ServerName: "x"}, utls.HelloChrome_Auto)
	if err := uc.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	chrome := uc.HandshakeState.Hello.Raw // Chrome offers h2 in ALPN
	if err := ValidateLoadedHello(chrome); err == nil {
		t.Error("an h2-ALPN hello must be rejected")
	}

	for _, bad := range [][]byte{nil, {0x16, 0x03}, []byte("not a hello")} {
		if err := ValidateLoadedHello(bad); err == nil {
			t.Errorf("garbage %v must be rejected", bad)
		}
	}
}

func TestNativeTLSHelloPreseed(t *testing.T) {
	srv, sess := newTestSessionForCreate(t)
	undici, err := os.ReadFile("testdata/undici_clienthello.bin")
	if err != nil {
		t.Fatal(err)
	}
	req := model.SessionRouteRequest{
		RouteClass: model.RouteClassFirstParty, RouteSource: model.RouteSourceExplicit,
		AuthMode: model.AuthModePassthrough, AuthSource: model.AuthSourceNone,
		Egress:     model.EgressConfig{Mode: "direct", Source: "test", Summary: "direct"},
		FailPolicy: model.FailClosed, NativeTLS: true, NativeTLSHello: undici,
	}
	if err := srv.setRoute(sess.public.ID, req); err != nil {
		t.Fatal(err)
	}
	p := sess.mirroredHelloRaw.Load()
	if p == nil || !bytes.Equal(*p, undici) {
		t.Fatal("loaded hello must pre-seed mirroredHelloRaw")
	}
	if !sess.nativeTLSHelloLoaded {
		t.Error("nativeTLSHelloLoaded must be true")
	}
	if !sess.snapshot().NativeTLSLoaded {
		t.Error("public NativeTLSLoaded must be true")
	}

	// A capture attempt now MUST no-op (mirroredHelloRaw already set) — loaded bytes survive.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_ = captureMirroredHello(sess, a) // returns immediately via the idempotency guard; conn unused
	if p2 := sess.mirroredHelloRaw.Load(); p2 == nil || !bytes.Equal(*p2, undici) {
		t.Error("capture must not overwrite a pre-seeded loaded hello")
	}
}
