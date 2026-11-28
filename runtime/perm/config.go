package perm

import (
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
)

// FromConfig builds a Rules from the permission lists in the runtime
// config (card ELR-102): the allow and deny rule lists are copied into
// perm.Rules, converting each config.PermRule to a perm.Rule. The lists
// are copies, not shares — mutating the returned Rules (or the config)
// leaves the other untouched. perm imports config, never the reverse, so
// the config package stays free of any knowledge of the permission
// decision layer.
func FromConfig(c config.Config) Rules {
	return Rules{
		Allow: copyRules(c.Permissions.Allow),
		Deny:  copyRules(c.Permissions.Deny),
	}
}

// copyRules copies in into a fresh slice of Rule. An empty (or nil) input
// yields an empty, non-nil slice.
func copyRules(in []config.PermRule) []Rule {
	out := make([]Rule, len(in))
	for i, p := range in {
		out[i] = Rule{Tool: p.Tool, Pattern: p.Pattern}
	}
	return out
}
