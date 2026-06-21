package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRequestBodyRefOmitemptyAndRoundTrip(t *testing.T) {
	b, _ := json.Marshal(RequestRecord{Method: "POST"})
	if strings.Contains(string(b), "body_ref") {
		t.Fatalf("nil BodyRef must be omitted: %s", b)
	}
	rec := RequestRecord{
		Method:  "POST",
		BodyRef: &RequestBodyRef{ID: "r1", Size: 42, SHA256: "abc", CapturedAt: time.Unix(1, 0).UTC()},
	}
	out, _ := json.Marshal(rec)
	if !strings.Contains(string(out), `"body_ref"`) || !strings.Contains(string(out), `"sha256":"abc"`) {
		t.Fatalf("BodyRef must serialize: %s", out)
	}
	var back RequestRecord
	if err := json.Unmarshal(out, &back); err != nil || back.BodyRef == nil || back.BodyRef.Size != 42 {
		t.Fatalf("round-trip failed: %v %+v", err, back.BodyRef)
	}
	if ControlAPIVersion != "v1" {
		t.Fatalf("ControlAPIVersion must stay v1 (additive change), got %q", ControlAPIVersion)
	}
}

// TestSessionCaptureBodiesJSON locks the wire shape for the per-session
// captureBodies flag that the inspect ribbon's Bodies cell reads + the
// runtime /capture/bodies toggle reports. Defaults to off ⇒ omitempty so
// pre-toggle wire shape is unchanged; flipped to on ⇒ "capture_bodies":true.
func TestSessionCaptureBodiesJSON(t *testing.T) {
	t.Run("omitempty-when-false", func(t *testing.T) {
		s := Session{ID: "s1", State: "running"}
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), "capture_bodies") {
			t.Fatalf("CaptureBodies=false must be omitted: %s", b)
		}
	})
	t.Run("emitted-when-true", func(t *testing.T) {
		s := Session{ID: "s1", State: "running", CaptureBodies: true}
		b, _ := json.Marshal(s)
		if !strings.Contains(string(b), `"capture_bodies":true`) {
			t.Fatalf("CaptureBodies=true must serialize as capture_bodies:true: %s", b)
		}
	})
}

// TestSessionCaptureBodiesUnmaskedJSON locks the omitempty wire shape for
// the CCWRAP_UNMASK_CREDENTIALS=1 surface field. Most users never enable the
// env so the field MUST be omitted by default (keeps the existing wire
// surface unchanged); emitted only when explicitly true so the inspect
// ribbon can render the danger marker.
func TestSessionCaptureBodiesUnmaskedJSON(t *testing.T) {
	t.Run("omitempty-when-false", func(t *testing.T) {
		s := Session{ID: "s1", State: "running"}
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), "capture_bodies_unmasked") {
			t.Fatalf("CaptureBodiesUnmasked=false must be omitted: %s", b)
		}
	})
	t.Run("emitted-when-true", func(t *testing.T) {
		s := Session{ID: "s1", State: "running", CaptureBodiesUnmasked: true}
		b, _ := json.Marshal(s)
		if !strings.Contains(string(b), `"capture_bodies_unmasked":true`) {
			t.Fatalf("CaptureBodiesUnmasked=true must serialize: %s", b)
		}
	})
}

func TestRelaunchClassConstants(t *testing.T) {
	if RelaunchLive != "live" {
		t.Fatalf("RelaunchLive = %q, want \"live\"", RelaunchLive)
	}
	if RelaunchNeedsRelaunch != "needs_relaunch" {
		t.Fatalf("RelaunchNeedsRelaunch = %q, want \"needs_relaunch\"", RelaunchNeedsRelaunch)
	}
	var rc RelaunchClass = RelaunchLive
	_ = rc
}
