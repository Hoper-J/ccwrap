package main

// doctorGroupExplicit maps every doctor check name to its display
// group. This is a literal lookup, NOT a name-prefix heuristic — the
// real names share no group-identifying prefix.
var doctorGroupExplicit = map[string]string{
	"paths":                            "Runtime",
	"ca":                               "Runtime",
	"session_listener":                 "Runtime",
	"update":                           "Runtime",
	"profile":                          "Provider + Auth",
	"effective_upstream":               "Provider + Auth",
	"upstream_inputs":                  "Provider + Auth",
	"inherited_upstream":               "Provider + Auth",
	"provider_selection":               "Provider + Auth",
	"auth_sources":                     "Provider + Auth",
	"parent_env_auth_sources":          "Provider + Auth",
	"hidden_auth_contract":             "Provider + Auth",
	"unsupported_env":                  "Provider + Auth",
	"settings_inspection":              "Settings inspection",
	"active_setting_sources":           "Settings inspection",
	"settings_unsupported_env":         "Settings inspection",
	"settings_malformed_env":           "Settings inspection",
	"settings_overridden_network_env":  "Settings inspection",
	"settings_policy_network_env":      "Settings inspection",
	"api_key_helper":                   "Settings inspection",
	"dangerous_shell_settings":         "Settings inspection",
	"custom_headers_auth":              "Settings inspection",
	"ccwrap_internal_keys_in_settings": "Settings inspection",
	"flag_settings":                    "Settings inspection",
	"egress_proxy":                     "Settings inspection",
	"discovery":                        "Discovery + launch",
	"launch_contract":                  "Discovery + launch",
	"session":                          "Session",
}

// doctorGroupOrder is the fixed display order.
var doctorGroupOrder = []string{
	"Runtime", "Provider + Auth", "Settings inspection",
	"Discovery + launch", "Session",
}

// doctorGroupForCheck returns the group for a check name; unknown
// names fall back to Runtime so a future check can never vanish.
func doctorGroupForCheck(name string) string {
	if g, ok := doctorGroupExplicit[name]; ok {
		return g
	}
	return "Runtime"
}
