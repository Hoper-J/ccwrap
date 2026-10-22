package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// --- Timezone alignment ------------------------------------------------------
//
// Claude Code stamps "Today's date is <YYYY-MM-DD>." into its request system
// prompt, computed from the LOCAL timezone. A China-local session therefore
// leaks a +8h date that can be one day ahead of a US "first-party" session and
// de-anonymizes its region. ccwrap does not scrub TZ, so setting env["TZ"] on
// the child aligns that date. These helpers resolve the effective TZ to inject
// (flag/env → persisted → first-run China-TZ prompt), validate it, and persist
// the first-run decision so it applies on every future launch without
// re-prompting.

// defaultInjectTimezone is offered as the Enter-default in the first-run prompt.
const defaultInjectTimezone = "America/Los_Angeles"

// timezoneSkipSentinel is the value persisted to <stateDir>/timezone when the
// user declines injection. It is a deliberately non-IANA token: reading it back
// suppresses both re-prompting and injection. time.LoadLocation is never called
// on it (the sentinel is mapped to "no injection" before validation).
const timezoneSkipSentinel = "none"

// chinaZoneNames are the IANA zone names that resolve to China Standard Time.
var chinaZoneNames = map[string]bool{
	"Asia/Shanghai":  true,
	"Asia/Urumqi":    true,
	"Asia/Chongqing": true,
	"Asia/Harbin":    true,
	"Asia/Kashgar":   true,
}

// isChinaTimezone reports whether the local zone looks like China. It gates ONLY
// the first-run prompt, so a loose heuristic is fine: a known China IANA name
// (from the TZ env or the /etc/localtime symlink target) matches directly;
// otherwise a current local UTC offset of exactly +8h (28800s) is a weak signal.
// An empty name with a non-+8h offset is not China.
func isChinaTimezone(zoneName string, offsetSeconds int) bool {
	if name := strings.TrimSpace(zoneName); name != "" {
		return chinaZoneNames[name]
	}
	return offsetSeconds == 28800
}

// detectLocalTimezone returns the local zone name (TZ env, else the IANA name
// parsed from the /etc/localtime symlink target) and the current local UTC
// offset in seconds (used only for the weak +8h fallback in isChinaTimezone).
func detectLocalTimezone(parentEnv []string) (string, int) {
	name := strings.TrimSpace(lookupEnv(parentEnv, "TZ"))
	if name == "" {
		name = zoneNameFromLocaltimeLink()
	}
	_, offset := time.Now().Zone()
	return name, offset
}

// zoneNameFromLocaltimeLink reads /etc/localtime and, if it is a symlink into a
// zoneinfo tree ("/usr/share/zoneinfo/Asia/Shanghai"), returns the IANA suffix
// ("Asia/Shanghai"). Returns "" when /etc/localtime is not a zoneinfo symlink.
func zoneNameFromLocaltimeLink() string {
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	const marker = "/zoneinfo/"
	if idx := strings.LastIndex(target, marker); idx >= 0 {
		return target[idx+len(marker):]
	}
	return ""
}

// timezonePersistPath is the small per-user file that records the first-run
// timezone decision (a chosen IANA name, or timezoneSkipSentinel).
func timezonePersistPath(stateDir string) string {
	return filepath.Join(stateDir, "timezone")
}

// readPersistedTimezone returns the persisted decision, or "" when there is no
// decision yet. Read errors (missing file, unreadable) are tolerated as "no
// decision" so a first run is never blocked by a bad state dir.
func readPersistedTimezone(stateDir string) string {
	data, err := os.ReadFile(timezonePersistPath(stateDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writePersistedTimezone records the decision, creating the state dir if needed.
func writePersistedTimezone(stateDir, value string) error {
	path := timezonePersistPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value+"\n"), 0o644)
}

// resolveInjectedTimezone returns the first non-empty of, in precedence order:
// the --timezone flag, the CCWRAP_TZ env, the persisted decision, then the
// first-run prompt result. "" means no override. It is pure (no I/O, no
// validation) so precedence is unit-testable in isolation; the caller validates
// and interprets the skip sentinel.
func resolveInjectedTimezone(flag, env, persisted, promptResult string) string {
	for _, v := range []string{flag, env, persisted, promptResult} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// timezonePromptDisabled gates the first-run timezone prompt. It reuses the
// profile-migration opt-out (--no-init / CCWRAP_NO_INIT) and adds a dedicated
// CCWRAP_NO_TZ_PROMPT for users who want profile migration but not this prompt.
func timezonePromptDisabled(parentEnv []string, noInit bool) bool {
	if migrationDisabled(parentEnv, noInit) {
		return true
	}
	return truthyEnv(lookupEnv(parentEnv, "CCWRAP_NO_TZ_PROMPT"))
}

// promptTimezoneDecision prints the date-leak explanation and reads one line
// from stdin, returning the decision: Enter → defaultInjectTimezone, "n"/"no"
// (case-insensitive) → timezoneSkipSentinel, any other text → that text (a
// candidate IANA name, validated later by the caller). EOF (Ctrl-D) is treated
// as skip so the prompt persists a decision and never nags again.
func promptTimezoneDecision(stdin io.Reader) string {
	fmt.Fprintln(os.Stderr, "ccwrap: 检测到当前为中国时区，Claude Code 会把本机当天日期写进请求，")
	fmt.Fprintln(os.Stderr, "ccwrap:   这可能暴露会话所在地区（+8 时区的日期可能比美国早一天）。")
	fmt.Fprintln(os.Stderr, "ccwrap:   (Detected a China timezone; the stamped date can reveal your region.)")
	fmt.Fprintln(os.Stderr, "ccwrap:   是否为 Claude Code 指定时区（让请求里的日期按该时区计算）？")
	fmt.Fprintf(os.Stderr, "ccwrap:   回车=%s，或输入一个 IANA 时区名，输入 n 跳过：", defaultInjectTimezone)
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		return timezoneSkipSentinel // EOF → persist skip, never re-prompt
	}
	answer := strings.TrimSpace(scanner.Text())
	switch strings.ToLower(answer) {
	case "":
		return defaultInjectTimezone
	case "n", "no":
		return timezoneSkipSentinel
	default:
		return answer
	}
}

// maybePromptTimezone runs the first-run China-timezone prompt and returns the
// user's decision (an IANA name, or timezoneSkipSentinel), persisting it so
// future launches never re-prompt. It returns "" without prompting or
// persisting when any precondition fails: opted out, non-TTY stdin, a decision
// already persisted, or the local zone is not China. stdin is read for the
// answer; the TTY gate goes through the isTerminalFn seam (stubbed in tests).
func maybePromptTimezone(stateDir string, parentEnv []string, noInit bool, stdin io.Reader) string {
	if timezonePromptDisabled(parentEnv, noInit) {
		return ""
	}
	if !isTerminalFn(os.Stdin) {
		return ""
	}
	if readPersistedTimezone(stateDir) != "" {
		return "" // already decided on a prior run
	}
	zoneName, offset := detectLocalTimezone(parentEnv)
	if !isChinaTimezone(zoneName, offset) {
		return ""
	}
	decision := promptTimezoneDecision(stdin)
	if err := writePersistedTimezone(stateDir, decision); err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: could not persist timezone decision: %v\n", err)
	}
	return decision
}

// resolveEffectiveTimezone resolves and validates the TZ to inject into the
// child. flagOrEnvTZ is launch.Timezone (--timezone over CCWRAP_TZ, collapsed by
// parseLaunchArgs). It consults the persisted decision and, only when nothing
// explicit or persisted exists, the first-run prompt. The result is validated
// via time.LoadLocation: an invalid zone warns on stderr and is dropped (no
// injection). The skip sentinel maps to "" (no injection). "" means the child
// env is byte-identical to today's.
func resolveEffectiveTimezone(stateDir, flagOrEnvTZ string, parentEnv []string, noInit bool) string {
	persisted := readPersistedTimezone(stateDir)
	var promptResult string
	if strings.TrimSpace(flagOrEnvTZ) == "" && persisted == "" {
		promptResult = maybePromptTimezone(stateDir, parentEnv, noInit, os.Stdin)
	}
	// env slot stays empty: parseLaunchArgs already folded CCWRAP_TZ into
	// flagOrEnvTZ (flag winning), so the precedence flag>env is settled upstream.
	resolved := resolveInjectedTimezone(flagOrEnvTZ, "", persisted, promptResult)
	if resolved == "" || resolved == timezoneSkipSentinel {
		return ""
	}
	if _, err := time.LoadLocation(resolved); err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: ignoring invalid timezone %q (not loadable): %v\n", resolved, err)
		return ""
	}
	return resolved
}
