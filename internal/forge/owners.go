package forge

import (
	"slices"
	"strings"
)

// OwnerFilter keeps or drops threads by the owner of their repository or
// project, before any of their content is fetched. An owner is an account or
// an organization; on GitLab it is a project's top-level namespace.
type OwnerFilter struct {
	include []string
	exclude []string
}

// NewOwnerFilter keeps only include's owners when include is non-empty, and
// never exclude's. Owners compare without case, as forges compare them.
func NewOwnerFilter(include, exclude []string) OwnerFilter {
	return OwnerFilter{include: normalizeOwners(include), exclude: normalizeOwners(exclude)}
}

func normalizeOwners(owners []string) []string {
	out := make([]string, 0, len(owners))
	for _, owner := range owners {
		owner = strings.ToLower(strings.TrimSpace(owner))
		if owner != "" && !slices.Contains(out, owner) {
			out = append(out, owner)
		}
	}
	slices.Sort(out)
	return out
}

// Allows reports whether threads of owner's repositories are archived.
func (f OwnerFilter) Allows(owner string) bool {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if slices.Contains(f.exclude, owner) {
		return false
	}
	return len(f.include) == 0 || slices.Contains(f.include, owner)
}

// Include returns the owners the filter is limited to, lowercased; empty
// means every owner not excluded.
func (f OwnerFilter) Include() []string { return slices.Clone(f.include) }

// Exclude returns the owners the filter drops, lowercased.
func (f OwnerFilter) Exclude() []string { return slices.Clone(f.exclude) }

// fingerprint names the filter in the resume state: discovery that came so
// far under one filter starts over under another, so a newly allowed
// owner's history is found.
func (f OwnerFilter) fingerprint() string {
	return "include=" + strings.Join(f.include, ",") + ";exclude=" + strings.Join(f.exclude, ",")
}
