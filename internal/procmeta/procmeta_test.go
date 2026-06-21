package procmeta

import (
	"os"
	"testing"
)

func TestCurrentStartToken(t *testing.T) {
	tok, err := CurrentStartToken()
	if err != nil {
		t.Fatalf("CurrentStartToken() error = %v", err)
	}
	if tok == "" {
		t.Fatal("CurrentStartToken() returned empty token")
	}
}

func TestMatchesCurrentProcess(t *testing.T) {
	tok, err := CurrentStartToken()
	if err != nil {
		t.Fatalf("CurrentStartToken() error = %v", err)
	}
	exists, match, err := Matches(os.Getpid(), tok)
	if err != nil {
		t.Fatalf("Matches() error = %v", err)
	}
	if !exists || !match {
		t.Fatalf("Matches(current pid) = exists=%v match=%v, want true/true", exists, match)
	}
}

func TestStartTokenRejectsInvalidPID(t *testing.T) {
	if _, err := StartToken(-1); err == nil {
		t.Fatal("expected invalid pid error")
	}
}
