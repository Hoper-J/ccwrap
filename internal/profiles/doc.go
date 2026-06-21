// Package profiles implements ccwrap's named provider/model profiles
// (profiles.json) — the foundation of the Provider Profiles feature.
//
// Local-trust credential philosophy: a profile MAY carry an
// inline secret (Auth.Key) OR an env reference (Auth.KeyEnv). ccwrap reads
// the file; it NEVER rewrites secrets back into it. No keychain is
// mandated. The one hard invariant is the product's function, not a
// user restriction: every profile activation still passes the preflight
// hidden-auth / fail-closed validation, an invalid/unresolvable profile
// refuses to launch, and credential VALUES never leave the proxy
// boundary (never to HTML/JS, /recent, or any control response).
//
// Absent file => behavior is exactly today's (inherit-env); the
// zero-touch first launch is unchanged.
package profiles
