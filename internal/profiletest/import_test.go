package profiletest_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoSupervisorImport guarantees the probe package does NOT pull
// in internal/supervisor — verifying I8 (probe traffic does not
// appear in inspect web /recent) by construction rather than at
// runtime.
func TestNoSupervisorImport(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/Hoper-J/ccwrap/internal/profiletest").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	forbidden := []string{
		"github.com/Hoper-J/ccwrap/internal/supervisor",
		"github.com/Hoper-J/ccwrap/internal/control",
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, dep := range deps {
		for _, f := range forbidden {
			if strings.TrimSpace(dep) == f {
				t.Errorf("internal/profiletest must not depend on %s; transitive dep observed", f)
			}
		}
	}
}
