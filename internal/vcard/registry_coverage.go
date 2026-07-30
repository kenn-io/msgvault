package vcard

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/vcard/registry"
)

// HandlingStrategy describes how later profile/resource layers treat a
// registered vCard element. The syntax codec preserves every strategy.
type HandlingStrategy string

const (
	HandlingNative       HandlingStrategy = "native"
	HandlingDerived      HandlingStrategy = "derived"
	HandlingRelationship HandlingStrategy = "relationship"
	HandlingMetadata     HandlingStrategy = "metadata"
	HandlingPreserve     HandlingStrategy = "preserve"
	HandlingFraming      HandlingStrategy = "framing"
)

// Handling is the declared treatment of one registered vCard element.
type Handling struct {
	Strategy HandlingStrategy
	Notes    string
}

var propertyHandling = map[string]Handling{
	"BEGIN":         {Strategy: HandlingFraming},
	"END":           {Strategy: HandlingFraming},
	"SOURCE":        {Strategy: HandlingNative},
	"KIND":          {Strategy: HandlingNative},
	"XML":           {Strategy: HandlingPreserve},
	"FN":            {Strategy: HandlingNative},
	"N":             {Strategy: HandlingNative},
	"NICKNAME":      {Strategy: HandlingNative},
	"PHOTO":         {Strategy: HandlingNative},
	"BDAY":          {Strategy: HandlingNative},
	"ANNIVERSARY":   {Strategy: HandlingNative},
	"GENDER":        {Strategy: HandlingNative},
	"ADR":           {Strategy: HandlingNative},
	"TEL":           {Strategy: HandlingNative},
	"EMAIL":         {Strategy: HandlingNative},
	"IMPP":          {Strategy: HandlingNative},
	"LANG":          {Strategy: HandlingNative},
	"TZ":            {Strategy: HandlingNative},
	"GEO":           {Strategy: HandlingNative},
	"TITLE":         {Strategy: HandlingDerived},
	"ROLE":          {Strategy: HandlingDerived},
	"LOGO":          {Strategy: HandlingNative},
	"ORG":           {Strategy: HandlingDerived},
	"MEMBER":        {Strategy: HandlingRelationship},
	"RELATED":       {Strategy: HandlingRelationship},
	"CATEGORIES":    {Strategy: HandlingNative},
	"NOTE":          {Strategy: HandlingNative},
	"PRODID":        {Strategy: HandlingDerived},
	"REV":           {Strategy: HandlingDerived},
	"SOUND":         {Strategy: HandlingNative},
	"UID":           {Strategy: HandlingDerived},
	"CLIENTPIDMAP":  {Strategy: HandlingMetadata},
	"URL":           {Strategy: HandlingNative},
	"VERSION":       {Strategy: HandlingDerived},
	"KEY":           {Strategy: HandlingNative},
	"FBURL":         {Strategy: HandlingNative},
	"CALADRURI":     {Strategy: HandlingNative},
	"CALURI":        {Strategy: HandlingNative},
	"BIRTHPLACE":    {Strategy: HandlingNative},
	"DEATHPLACE":    {Strategy: HandlingNative},
	"DEATHDATE":     {Strategy: HandlingNative},
	"EXPERTISE":     {Strategy: HandlingNative},
	"HOBBY":         {Strategy: HandlingNative},
	"INTEREST":      {Strategy: HandlingNative},
	"ORG-DIRECTORY": {Strategy: HandlingNative},
	"CONTACT-URI":   {Strategy: HandlingNative},
	"CREATED":       {Strategy: HandlingDerived},
	"GRAMGENDER":    {Strategy: HandlingNative},
	"LANGUAGE":      {Strategy: HandlingNative},
	"PRONOUNS":      {Strategy: HandlingNative},
	"SOCIALPROFILE": {Strategy: HandlingNative},
	"JSPROP":        {Strategy: HandlingPreserve},
}

var parameterHandling = preservedHandling(
	"LANGUAGE", "VALUE", "PREF", "ALTID", "PID", "TYPE", "MEDIATYPE",
	"CALSCALE", "SORT-AS", "GEO", "TZ", "INDEX", "LEVEL", "GROUP", "CC",
	"AUTHOR", "AUTHOR-NAME", "CREATED", "DERIVED", "LABEL", "PHONETIC",
	"PROP-ID", "SCRIPT", "SERVICE-TYPE", "USERNAME", "JSPTR", "JSCOMPS",
)

// PropertyHandling returns the declared handling for a registered property.
func PropertyHandling(name string) (Handling, bool) {
	handling, ok := propertyHandling[strings.ToUpper(strings.TrimSpace(name))]
	return handling, ok
}

// ParameterHandling returns the declared handling for a registered parameter.
func ParameterHandling(name string) (Handling, bool) {
	handling, ok := parameterHandling[strings.ToUpper(strings.TrimSpace(name))]
	return handling, ok
}

// ValidateRegistryCoverage checks the package declarations against a snapshot.
func ValidateRegistryCoverage(snapshot registry.Snapshot) error {
	return validateRegistryCoverage(snapshot, propertyHandling, parameterHandling)
}

func preservedHandling(names ...string) map[string]Handling {
	handling := make(map[string]Handling, len(names))
	for _, name := range names {
		handling[name] = Handling{Strategy: HandlingPreserve}
	}
	return handling
}

func validateRegistryCoverage(
	snapshot registry.Snapshot,
	properties map[string]Handling,
	parameters map[string]Handling,
) error {
	var diagnostics []string
	propertyNames := elementNameSet(snapshot.Properties)
	parameterNames := elementNameSet(snapshot.Parameters)

	for name := range propertyNames {
		handling, ok := properties[name]
		if !ok {
			diagnostics = append(diagnostics, "missing property "+name)
			continue
		}
		if !validHandlingStrategy(handling.Strategy) {
			diagnostics = append(diagnostics, fmt.Sprintf(
				"property %s has invalid handling strategy %q",
				name, handling.Strategy,
			))
		}
	}
	for name := range properties {
		if _, ok := propertyNames[name]; !ok {
			diagnostics = append(diagnostics, "stale property "+name)
		}
	}

	for name := range parameterNames {
		handling, ok := parameters[name]
		if !ok {
			diagnostics = append(diagnostics, "missing parameter "+name)
			continue
		}
		if !validHandlingStrategy(handling.Strategy) {
			diagnostics = append(diagnostics, fmt.Sprintf(
				"parameter %s has invalid handling strategy %q",
				name, handling.Strategy,
			))
		}
	}
	for name := range parameters {
		if _, ok := parameterNames[name]; !ok {
			diagnostics = append(diagnostics, "stale parameter "+name)
		}
	}

	if len(diagnostics) == 0 {
		return nil
	}
	sort.Strings(diagnostics)
	return errors.New(strings.Join(diagnostics, "; "))
}

func elementNameSet(elements []registry.Element) map[string]struct{} {
	names := make(map[string]struct{}, len(elements))
	for _, element := range elements {
		names[strings.ToUpper(element.Name)] = struct{}{}
	}
	return names
}

func validHandlingStrategy(strategy HandlingStrategy) bool {
	switch strategy {
	case HandlingNative,
		HandlingDerived,
		HandlingRelationship,
		HandlingMetadata,
		HandlingPreserve,
		HandlingFraming:
		return true
	default:
		return false
	}
}
