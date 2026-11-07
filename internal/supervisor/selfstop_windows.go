package supervisor

import "errors"

// canStopSelf is false: Windows has no signal a process can send itself
// without Claude Code, which shares its console, getting it too.
const canStopSelf = false

func stopSelf() error {
	return errors.ErrUnsupported
}
