package procgroup

import (
	"os"
	"os/signal"
	"testing"
	"time"
)

// The copy Respawn starts leads a group of its own with Ctrl+C on: its
// Interrupt reaches it, and not this process. The copy is this test again.
func TestRespawnLeadsAGroupWithCtrlCOn(t *testing.T) {
	if Lead() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		if os.Getenv(leaderEnv) != "" {
			os.Exit(5) // the marker would reach Claude Code
		}
		if err := Interrupt(); err != nil {
			os.Exit(3)
		}
		select {
		case <-sig:
			os.Exit(7)
		case <-time.After(5 * time.Second):
			os.Exit(4) // Ctrl+C still off, or sent elsewhere
		}
	}
	got := make(chan os.Signal, 1)
	signal.Notify(got, os.Interrupt)
	defer signal.Stop(got)
	bound := false
	code, ok := Respawn([]string{"-test.run=^TestRespawnLeadsAGroupWithCtrlCOn$"}, func(p *os.Process) { bound = p != nil })
	if !ok {
		t.Skip("no console")
	}
	if code != 7 || !bound {
		t.Fatalf("copy exited %d (7 = got its own Ctrl+C), bound=%v", code, bound)
	}
	select {
	case <-got:
		t.Fatal("the copy's Ctrl+C reached this process")
	case <-time.After(300 * time.Millisecond):
	}
}
