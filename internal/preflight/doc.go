// Package preflight runs the launch resolution: it reads on-disk settings via
// settings.InspectLaunch, merges with Options-supplied launch inputs, and
// produces a *Result describing the resolved routing/auth/aliases/headers/egress.
//
// Live hot-swap extends Options with file-content snapshot fields for
// byte-faithful switch resolution (see Options doc-comments). The supervisor
// retains a *LaunchContext{Options, Inspection, *Result} at launch and reuses
// it on every SwitchProfile, with only Options.Profile swapped. The resolver
// runs the SAME code path on switch as at launch; only disk I/O for file-backed
// inputs is bypassed via the content-snapshot fields.
//
// # ParentEnv immutability invariant
//
// Options.ParentEnv []string is captured fresh at cmd/ccwrap/main.go (via os.Environ()
// which always returns a new slice) and is treated as read-only thereafter.
// No code in this package, in internal/modelalias, in internal/upstreamheaders,
// in internal/supervisor, or in cmd/ccwrap mutates the slice. applyProfileOverlay
// builds a defensive copy when it needs to append env entries:
//
//	merged := append([]string(nil), opts.ParentEnv...)
//	merged = append(merged, "KEY=VALUE")
//	// opts.ParentEnv unaffected
//
// Any future code that needs to mutate the env tier MUST take a defensive copy
// first. Appending directly to Options.ParentEnv is forbidden.
package preflight
