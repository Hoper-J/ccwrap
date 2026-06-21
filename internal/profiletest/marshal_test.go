package profiletest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMarshalResult_OK(t *testing.T) {
	r := ProbeResult{
		Profile:     "glm",
		Status:      StatusOK,
		Latency:     720 * time.Millisecond,
		HTTPStatus:  200,
		BaseURLHost: "open.bigmodel.cn",
		ModelSent:   "glm-4-flash",
		ModelEchoed: "glm-4-flash",
	}
	data, err := MarshalResult(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	if got["profile"] != "glm" {
		t.Errorf("profile: %v", got["profile"])
	}
	if got["status"] != "OK" {
		t.Errorf("status: %v", got["status"])
	}
	if got["latency_ms"].(float64) != 720 {
		t.Errorf("latency_ms: %v", got["latency_ms"])
	}
	if got["http_status"].(float64) != 200 {
		t.Errorf("http_status: %v", got["http_status"])
	}
	if got["base_url_host"] != "open.bigmodel.cn" {
		t.Errorf("base_url_host: %v", got["base_url_host"])
	}
	if got["model_sent"] != "glm-4-flash" {
		t.Errorf("model_sent: %v", got["model_sent"])
	}
	if got["model_echoed"] != "glm-4-flash" {
		t.Errorf("model_echoed: %v", got["model_echoed"])
	}
	if got["error"] != nil {
		t.Errorf("error must be null on OK; got %v", got["error"])
	}
	if got["skipped_reason"] != nil {
		t.Errorf("skipped_reason must be null on OK; got %v", got["skipped_reason"])
	}
}

func TestMarshalResult_AliasRewrite(t *testing.T) {
	r := ProbeResult{
		Profile:              "glm",
		Status:               StatusOK,
		Latency:              100 * time.Millisecond,
		HTTPStatus:           200,
		BaseURLHost:          "open.bigmodel.cn",
		ModelSent:            "glm-4-flash",
		ModelSentRewroteFrom: "haiku",
		ModelEchoed:          "glm-4-flash",
	}
	data, _ := MarshalResult(r)
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	if got["model_sent"] != "haiku" {
		t.Errorf("model_sent (alias input): got %v, want haiku", got["model_sent"])
	}
	if got["model_sent_rewritten_to"] != "glm-4-flash" {
		t.Errorf("model_sent_rewritten_to: got %v", got["model_sent_rewritten_to"])
	}
}

func TestMarshalResult_Skipped(t *testing.T) {
	r := ProbeResult{
		Profile:       "oauth",
		Status:        StatusSkipped,
		BaseURLHost:   "api.anthropic.com",
		SkippedReason: "passthrough: CCWRAP does not own credential",
	}
	data, _ := MarshalResult(r)
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	if got["latency_ms"] != nil {
		t.Errorf("latency_ms must be null for SKIPPED; got %v", got["latency_ms"])
	}
	if got["model_sent"] != nil {
		t.Errorf("model_sent must be null for SKIPPED; got %v", got["model_sent"])
	}
	if !strings.Contains(got["skipped_reason"].(string), "passthrough") {
		t.Errorf("skipped_reason: %v", got["skipped_reason"])
	}
}

func TestMarshalResult_AuthFail(t *testing.T) {
	r := ProbeResult{
		Profile:     "kimi",
		Status:      StatusAuthFail,
		Latency:     45 * time.Millisecond,
		HTTPStatus:  401,
		BaseURLHost: "api.moonshot.cn",
		ModelSent:   "claude-haiku-4-5-20251001",
		Err:         "401 Unauthorized",
	}
	data, _ := MarshalResult(r)
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	if got["status"] != "AUTH_FAIL" {
		t.Errorf("status: %v", got["status"])
	}
	if got["error"].(string) != "401 Unauthorized" {
		t.Errorf("error: %v", got["error"])
	}
	if got["model_echoed"] != nil {
		t.Errorf("model_echoed must be null when no echo; got %v", got["model_echoed"])
	}
}
