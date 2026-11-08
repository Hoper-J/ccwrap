package supervisor

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/procgroup"
)

// stopSelf asks the session's Claude Code to exit by itself. Windows has no
// SIGTERM; a console program gets a Ctrl+C instead, sent to the session's
// own process group (see procgroup), and the launcher gives Claude Code the
// grace it gives a SIGTERM elsewhere. A launch outside such a group, or a
// failed Ctrl+C, terminates Claude Code, skipping its exit handling. Without
// a live Claude Code (not attached yet, or its pid now someone else's) only
// the supervisor is left to stop.
func (s *Supervisor) stopSelf() error {
	for _, sess := range s.listSessions() {
		if sess.State == model.StateEnded || sess.ClaudePID <= 0 {
			continue
		}
		if _, match, err := procmetaMatches(sess.ClaudePID, sess.ClaudeStartToken); err != nil || !match {
			continue
		}
		if interruptGroup() == nil {
			return nil
		}
		p, err := os.FindProcess(sess.ClaudePID)
		if err != nil {
			return err
		}
		return p.Kill()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}

// interruptGroup sends the Ctrl+C when this process leads the session's
// group; tests replace it.
var interruptGroup = func() error {
	if !procgroup.Leader() {
		return errors.New("not leading a process group")
	}
	return procgroup.Interrupt()
}
