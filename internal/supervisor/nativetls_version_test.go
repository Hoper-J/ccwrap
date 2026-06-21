package supervisor

import (
	"os"
	"strings"
	"testing"
)

// utlsPinnedVersion is asserted here so a go.mod bump that is not consciously
// reviewed (the fingerprint mirror depends on utls behavior) fails the build.
func TestUTLSVersionPinned(t *testing.T) {
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	want := "github.com/refraction-networking/utls " + utlsPinnedVersion
	if !strings.Contains(string(b), want) {
		t.Fatalf("go.mod must pin %q; bump the dependency AND utlsPinnedVersion together", want)
	}
}
