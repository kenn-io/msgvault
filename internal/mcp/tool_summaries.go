package mcp

import (
	"cmp"
	"slices"
)

type securityPolicy struct {
	name    string
	flag    string
	enabled bool
}

// securityPolicyFor keeps documentation and registration on the same class
// policy. Backend availability, transport and delegated grants are separate.
func securityPolicyFor(class toolSecurityClass, opts ServeOptions) securityPolicy {
	switch class {
	case toolSecurityRead:
		return securityPolicy{"read", "", true}
	case toolSecurityWrite:
		return securityPolicy{"write", "", true}
	case toolSecurityProfileWrite:
		return securityPolicy{"profile_write", "--allow-profile-writes", opts.AllowProfileWrites}
	case toolSecurityDeliveryPolicyWrite:
		return securityPolicy{"delivery_policy_write", "--allow-delivery-policy-writes", opts.AllowDeliveryPolicyWrites}
	case toolSecurityIdentityDecision:
		return securityPolicy{"identity_decision", "--allow-identity-decisions", opts.AllowIdentityDecisions}
	case toolSecurityIdentityScoring:
		return securityPolicy{"identity_scoring", "--allow-identity-scoring", opts.AllowIdentityScoring}
	case toolSecurityPersonMerge:
		return securityPolicy{"person_merge", "--allow-person-merges", opts.AllowPersonMerges}
	case toolSecurityCardDAVWrite:
		return securityPolicy{"carddav_write", "--allow-carddav-writes", opts.AllowCardDAVWrites}
	case toolSecurityCalendarWrite:
		return securityPolicy{"calendar_write", "--allow-calendar-writes", opts.AllowCalendarWrites}
	case toolSecurityKataWrite:
		return securityPolicy{"kata_write", "--allow-kata-writes", opts.AllowKataWrites}
	default:
		return securityPolicy{"unknown", "", false}
	}
}

func securityFlag(class toolSecurityClass) string {
	return securityPolicyFor(class, ServeOptions{}).flag
}

func toolClassAllowed(class toolSecurityClass, opts ServeOptions, allowWrites bool) bool {
	return (class == toolSecurityRead || allowWrites) && securityPolicyFor(class, opts).enabled
}

// ToolSummary describes a registered definition, not a running session's access.
// GrantPermission is owner-only unless the read gate names a permission, or a
// calendar/draft backend supplies its own source and action authorization.
type ToolSummary struct {
	Name            string
	Description     string
	Security        string
	Flag            string
	GrantPermission string
}

func summarizeTool(definition toolDefinition) ToolSummary {
	permission := "owner-only"
	if required, ok := agentReadToolPermission(definition.name); ok {
		permission = string(required)
	}
	return ToolSummary{
		Name: definition.name, Description: definition.description,
		Security: securityPolicyFor(definition.security, ServeOptions{}).name,
		Flag:     securityFlag(definition.security), GrantPermission: permission,
	}
}

// ToolSummaries returns all static definitions in class/name order. It neither
// opens an archive nor creates provider clients. Use tools/list for the actual
// tools available to a particular connection and credential.
func ToolSummaries() []ToolSummary {
	definitions := operationDefinitions(catalogCapabilities{kataDocbank: true})
	definitions = append(definitions, stableIdentityScoringDefinitions...)
	summaries := make([]ToolSummary, 0, len(definitions))
	for _, definition := range definitions {
		summaries = append(summaries, summarizeTool(definition))
	}
	for _, definition := range stableCalendarTools() {
		summary := summarizeTool(definition)
		summary.GrantPermission = "backend-dependent"
		summaries = append(summaries, summary)
	}
	for _, definition := range stableDraftDefinitions {
		summary := summarizeTool(definition)
		summary.GrantPermission = "backend-dependent"
		summaries = append(summaries, summary)
	}
	for _, operation := range fixedOperationalDefinitions() {
		summary := summarizeTool(operation.definition)
		if operation.writes {
			switch operation.family {
			case OperationFamilyRecords:
				summary.Security = securityPolicyFor(toolSecurityProfileWrite, ServeOptions{}).name
				summary.Flag = securityFlag(toolSecurityProfileWrite)
			case OperationFamilySources:
				summary.Security, summary.Flag = "source_write", "--allow-source-writes"
			}
		}
		summaries = append(summaries, summary)
	}
	slices.SortFunc(summaries, func(a, b ToolSummary) int {
		return cmp.Or(cmp.Compare(a.Security, b.Security), cmp.Compare(a.Name, b.Name))
	})
	return summaries
}
