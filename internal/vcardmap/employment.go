// Package vcardmap projects native Msgvault records onto vCard properties.
//
// The employment helpers in this file produce unescaped component values that
// the vCard codec escapes, folds, and renders. The person projection
// (ProjectPersonEnvelope) goes further: it renders a semantic snapshot into
// managed property occurrences and merges them into a vcard.ResourceEnvelope,
// so it depends on the vcard package for property construction, escaping, and
// envelope merging, and on google/uuid to recognise counterpart UIDs.
package vcardmap

import (
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// unitSeparators are the characters a user may type to nest organizational
// units inside one department field.
const unitSeparators = "/>"

// Employment is the subset of a derived employment projection that vCard ORG,
// TITLE, and ROLE need.
type Employment struct {
	OrganizationName string
	Department       string
	Title            string
	Role             string
}

// FromProjection adapts the store's derived primary current employment. A
// zero projection, which is what the store returns when a person has no
// primary current employment, yields a zero Employment and therefore no ORG,
// TITLE, or ROLE value.
func FromProjection(projection store.EmploymentProjection) Employment {
	return Employment{
		OrganizationName: projection.OrganizationName,
		Department:       projection.Department,
		Title:            projection.Title,
		Role:             projection.Role,
	}
}

// OrgComponents returns the vCard ORG structured value: the organization name
// followed by its organizational unit names, per RFC 6350 section 6.6.4.
// Returns nil when there is no organization name, because a unit without an
// organization is not a representable ORG value. Component values are
// unescaped.
func OrgComponents(employment Employment) []string {
	organization := strings.TrimSpace(employment.OrganizationName)
	if organization == "" {
		return nil
	}
	components := []string{organization}
	for _, unit := range strings.FieldsFunc(employment.Department, func(r rune) bool {
		return strings.ContainsRune(unitSeparators, r)
	}) {
		if unit = strings.TrimSpace(unit); unit != "" {
			components = append(components, unit)
		}
	}
	return components
}

// Title returns the vCard TITLE value for the primary current employment, or
// "" when it has no title. The value is unescaped.
func Title(employment Employment) string {
	return strings.TrimSpace(employment.Title)
}

// Role returns the vCard ROLE value for the primary current employment, or
// "" when it has no role. The value is unescaped.
func Role(employment Employment) string {
	return strings.TrimSpace(employment.Role)
}
