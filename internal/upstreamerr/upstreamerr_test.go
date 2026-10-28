package upstreamerr

import "testing"

func TestMessage(t *testing.T) {
	cases := []struct {
		name, body, typ, msg string
	}{
		{"anthropic", `{"type":"error","error":{"type":"not_found_error","message":"model: claude-fable-5-1"}}`, "not_found_error", "model: claude-fable-5-1"},
		{"openai", `{"error":{"message":"The model does not exist","type":"invalid_request_error","code":"model_not_found"}}`, "invalid_request_error", "The model does not exist"},
		{"error string", `{"error":"upstream timeout"}`, "", "upstream timeout"},
		{"message only", `{"message":"Service temporarily unavailable"}`, "", "Service temporarily unavailable"},
		{"cloudflare problem json", `{"type":"https://developers.cloudflare.com/errors/502/","title":"Error 502: Bad gateway","status":502,"detail":"The origin web server returned an invalid or incomplete response to Cloudflare."}`, "", "Error 502: Bad gateway: The origin web server returned an invalid or incomplete response to Cloudflare."},
		{"html", "<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>", "", "502 Bad Gateway"},
		{"plain text", "404 page not found\n", "", "404 page not found"},
		{"empty", "  ", "", ""},
	}
	for _, tc := range cases {
		typ, msg := Message([]byte(tc.body))
		if typ != tc.typ || msg != tc.msg {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, typ, msg, tc.typ, tc.msg)
		}
	}
}
