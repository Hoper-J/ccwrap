package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/uiservice"
)

// Sessions are looked at in the app (ccwrap ui). An interactive launch
// starts it in the background so its banner can link there; ccwrap open
// starts it and opens the session in it.

// noUIEnv set to 1 keeps a launch from starting the app; its banner then
// says to run ccwrap open, which starts the app regardless.
const noUIEnv = "CCWRAP_NO_UI"

// ensureUI starts ccwrap ui if it is not running (or replaces one from a
// different ccwrap) and waits up to wait for it; a seam for tests.
var ensureUI = func(paths app.Paths, wait time.Duration) (uiservice.Info, error) {
	exe, err := ccwrapExecutable()
	if err != nil {
		return uiservice.Info{}, err
	}
	// A test binary would run its tests as "ui serve".
	if strings.HasSuffix(filepath.Base(exe), ".test") {
		return uiservice.Info{}, errors.New("not starting ccwrap ui from a test binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait+3*time.Second)
	defer cancel()
	return uiservice.Ensure(ctx, paths, exe, versionString(), wait)
}

// uiStart is ccwrap ui being started in the background.
type uiStart struct {
	done chan struct{}
	info uiservice.Info
	err  error
}

// startUIInBackground starts ccwrap ui without waiting; nil when
// CCWRAP_NO_UI=1 or when no one would see the link (stderr is not a
// terminal).
func startUIInBackground(paths app.Paths, getenv func(string) string, interactive bool) *uiStart {
	if truthyEnv(getenv(noUIEnv)) || !interactive {
		return nil
	}
	s := &uiStart{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.info, s.err = ensureUI(paths, 3*time.Second)
	}()
	return s
}

// sessionLink is the session's address in the app if the app is up within
// patience, else "".
func (s *uiStart) sessionLink(id string, patience time.Duration) string {
	if s == nil {
		return ""
	}
	select {
	case <-s.done:
	case <-time.After(patience):
		return ""
	}
	if s.err != nil {
		return ""
	}
	return appSessionURL(s.info, id)
}

func appSessionURL(info uiservice.Info, id string) string {
	return info.URL() + "#/sessions/" + id
}
