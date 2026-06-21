package control

import "fmt"

// RouteError is the typed 409 response from /route control operations. A
// post-attach SetRoute is refused with this typed body so callers
// can programmatically detect the refusal (via errors.As) without parsing
// free-form error messages.
//
// Wire format on the control socket (HTTP 409 Conflict):
//
//	{"reason_code": "RouteSetupAfterAttach", "message": "<sanitized>"}
type RouteError struct {
	Code    string `json:"reason_code"`
	Message string `json:"message"`
}

// Error implements the error interface. The format keeps Code first (the
// programmatic key) and folds Message in when present. Stable enough for
// log lines; callers MUST use errors.As when they need the typed Code.
func (e *RouteError) Error() string {
	if e == nil {
		return "route error: <nil>"
	}
	if e.Message == "" {
		return fmt.Sprintf("route error: %s", e.Code)
	}
	return fmt.Sprintf("route error [%s]: %s", e.Code, e.Message)
}
