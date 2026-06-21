package profiletest

import "encoding/json"

// MarshalResult formats a ProbeResult as the wire JSON used by both
// the CLI (`ccwrap profile test --format json`) and the inspect-web
// `/profile/test` endpoint. *T pointer fields encode as `null` when
// the underlying value is the zero-value.
//
// Schema:
//
//	profile, status, latency_ms, http_status, base_url_host,
//	model_sent, model_sent_rewritten_to, model_echoed, error,
//	skipped_reason
func MarshalResult(r ProbeResult) ([]byte, error) {
	type wire struct {
		Profile              string  `json:"profile"`
		Status               string  `json:"status"`
		LatencyMs            *int64  `json:"latency_ms"`
		HTTPStatus           *int    `json:"http_status"`
		BaseURLHost          string  `json:"base_url_host"`
		ModelSent            *string `json:"model_sent"`
		ModelSentRewrittenTo *string `json:"model_sent_rewritten_to"`
		ModelEchoed          *string `json:"model_echoed"`
		Error                *string `json:"error"`
		SkippedReason        *string `json:"skipped_reason"`
	}
	row := wire{
		Profile:     r.Profile,
		Status:      r.Status.String(),
		BaseURLHost: r.BaseURLHost,
	}
	if r.Latency > 0 && r.Status != StatusSkipped {
		ms := r.Latency.Milliseconds()
		row.LatencyMs = &ms
	}
	if r.HTTPStatus != 0 {
		code := r.HTTPStatus
		row.HTTPStatus = &code
	}
	if r.ModelSent != "" {
		s := r.ModelSent
		row.ModelSent = &s
	}
	if r.ModelSentRewroteFrom != "" {
		// Alias-rewrite handling: model_sent = input (haiku);
		// model_sent_rewritten_to = output (glm-4-flash).
		to := r.ModelSent
		row.ModelSentRewrittenTo = &to
		from := r.ModelSentRewroteFrom
		row.ModelSent = &from
	}
	if r.ModelEchoed != "" {
		s := r.ModelEchoed
		row.ModelEchoed = &s
	}
	if r.Err != "" {
		s := r.Err
		row.Error = &s
	}
	if r.SkippedReason != "" {
		s := r.SkippedReason
		row.SkippedReason = &s
	}
	return json.Marshal(row)
}
