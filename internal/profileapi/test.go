package profileapi

import (
	"errors"
	"net/http"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// Why a profile could not be found for a test.
var (
	ErrNotFound          = errors.New("no such profile")
	ErrConfig            = errors.New("profile config error")
	ErrInheritEnvMissing = errors.New("inherit-env credentials missing")
)

// ProfileForTest is the profile a connection test probes: a saved one, or
// for inherit-env the route the environment describes.
func (s *Store) ProfileForTest(name string, getenv func(string) string) (profiles.Profile, error) {
	if name == profiles.InheritEnv {
		return ResolveInheritEnv(getenv)
	}
	f, err := s.Load()
	if err != nil {
		return profiles.Profile{}, ErrConfig
	}
	if f == nil || len(f.Profiles) == 0 {
		return profiles.Profile{}, ErrNotFound
	}
	p, ok := f.Profiles[name]
	if !ok {
		return profiles.Profile{}, ErrNotFound
	}
	p.Name = name
	return p, nil
}

// ResolveInheritEnv builds the inherit-env profile from ANTHROPIC_API_KEY
// (x-api-key) or ANTHROPIC_AUTH_TOKEN (bearer) and ANTHROPIC_BASE_URL.
func ResolveInheritEnv(getenv func(string) string) (profiles.Profile, error) {
	apiKey := getenv("ANTHROPIC_API_KEY")
	authTok := getenv("ANTHROPIC_AUTH_TOKEN")
	if apiKey == "" && authTok == "" {
		return profiles.Profile{}, ErrInheritEnvMissing
	}
	baseURL := getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	auth := &profiles.AuthSpec{}
	if apiKey != "" {
		auth.Mode = "ccwrap_x_api_key"
		auth.Key = apiKey
	} else {
		auth.Mode = "ccwrap_bearer"
		auth.Key = authTok
	}
	return profiles.Profile{Name: profiles.InheritEnv, BaseURL: baseURL, Auth: auth}, nil
}

// TestErrorStatus is the HTTP status and message for a ProfileForTest
// error. Unknown errors read as a bare internal error, so no detail leaks.
func TestErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "no such profile"
	case errors.Is(err, ErrInheritEnvMissing):
		return http.StatusNotFound, "inherit-env credentials missing"
	case errors.Is(err, ErrConfig):
		return http.StatusInternalServerError, "profile config error"
	}
	return http.StatusInternalServerError, "internal error"
}
