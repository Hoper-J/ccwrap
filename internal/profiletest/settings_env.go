package profiletest

import "github.com/Hoper-J/ccwrap/internal/settings"

// ClaudeSettingsEnv is base (KEY=value pairs, usually os.Environ()) with the
// proxy variables from the user's Claude settings laid over it: the env an
// inherit egress resolves against in a session, minus project settings,
// since a profile test has no project folder. fromSettings reports whether
// a settings file set any proxy variable.
func ClaudeSettingsEnv(base []string) (env map[string]string, fromSettings bool, err error) {
	inspect, err := settings.InspectLaunch("", nil)
	if err != nil {
		return nil, false, err
	}
	eff, err := settings.EffectiveProxyEnvFromInspection(base, inspect)
	if err != nil {
		return nil, false, err
	}
	return eff.Env, len(eff.ContributingSources) > 0, nil
}
