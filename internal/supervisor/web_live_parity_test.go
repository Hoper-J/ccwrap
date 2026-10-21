package supervisor

import (
	"bytes"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/ui"
)

func renderedInlineScript(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	return m[1]
}

// liftScriptFns returns the named inline-script functions verbatim.
func liftScriptFns(t *testing.T, js string, names ...string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range names {
		body := scriptFnBody(t, js, name)
		sig := "function " + name + "("
		i := strings.Index(js, sig)
		params := js[i+len(sig):]
		params = params[:strings.IndexByte(params, ')')]
		b.WriteString("function " + name + "(" + params + ")" + body + "\n")
	}
	return b.String()
}

func runNode(t *testing.T, script string) []byte {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return out
}
