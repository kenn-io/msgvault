package vcard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/vcard/registry"
)

// Version returns the card's single supported VERSION value.
func (c Card) Version() (Version, error) {
	versions := c.PropertiesNamed("VERSION")
	if len(versions) != 1 {
		return "", fmt.Errorf("requires exactly one VERSION, found %d", len(versions))
	}
	version := Version(strings.TrimSpace(versions[0].RawValue))
	switch version {
	case Version21, Version30, Version40:
		return version, nil
	default:
		return "", fmt.Errorf("unsupported VERSION %q", version)
	}
}

// Validate checks explicit cross-property vCard semantics.
func Validate(doc Document) error {
	if len(doc.Cards) == 0 {
		return errors.New("document contains no cards")
	}
	var diagnostics []string
	for cardIndex, card := range doc.Cards {
		prefix := fmt.Sprintf("card %d: ", cardIndex+1)
		version, versionErr := card.Version()
		if versionErr != nil {
			diagnostics = append(diagnostics, prefix+versionErr.Error())
		}

		hasFullName := false
		for propertyIndex, property := range card.Properties {
			if property.Name == "" {
				diagnostics = append(diagnostics,
					fmt.Sprintf("%sproperty %d has empty property name", prefix, propertyIndex+1))
			}
			if strings.EqualFold(property.Name, "FN") &&
				strings.TrimSpace(property.RawValue) != "" {
				hasFullName = true
			}
			for parameterIndex, parameter := range property.Parameters {
				if parameter.Name == "" {
					diagnostics = append(diagnostics, fmt.Sprintf(
						"%sproperty %d parameter %d has empty parameter name",
						prefix, propertyIndex+1, parameterIndex+1,
					))
					continue
				}
				switch {
				case strings.EqualFold(parameter.Name, "PREF") && len(parameter.Values) == 0:
					diagnostics = append(diagnostics, fmt.Sprintf(
						"%sproperty %d PREF must be an integer from 1 through 100",
						prefix, propertyIndex+1,
					))
				case strings.EqualFold(parameter.Name, "INDEX") && len(parameter.Values) == 0:
					diagnostics = append(diagnostics, fmt.Sprintf(
						"%sproperty %d INDEX must be a positive integer",
						prefix, propertyIndex+1,
					))
				}
				for _, value := range parameter.Values {
					switch {
					case strings.EqualFold(parameter.Name, "PREF"):
						if !integerInRange(value.Decoded, 1, 100) {
							diagnostics = append(diagnostics, fmt.Sprintf(
								"%sproperty %d PREF must be an integer from 1 through 100",
								prefix, propertyIndex+1,
							))
						}
					case strings.EqualFold(parameter.Name, "INDEX"):
						if !integerInRange(value.Decoded, 1, int(^uint(0)>>1)) {
							diagnostics = append(diagnostics, fmt.Sprintf(
								"%sproperty %d INDEX must be a positive integer",
								prefix, propertyIndex+1,
							))
						}
					}
				}
			}
		}
		if versionErr == nil && (version == Version30 || version == Version40) && !hasFullName {
			diagnostics = append(diagnostics, fmt.Sprintf(
				"%svCard %s requires at least one non-empty FN",
				prefix, version,
			))
		}
	}
	if len(diagnostics) > 0 {
		return fmt.Errorf("%s", strings.Join(diagnostics, "; "))
	}
	return nil
}

func integerInRange(raw string, minimum, maximum int) bool {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	return err == nil && value >= minimum && value <= maximum
}

// IsRegisteredPropertyValue reports case-insensitive registry membership.
func IsRegisteredPropertyValue(
	snapshot registry.Snapshot,
	property string,
	value string,
) bool {
	for _, registered := range snapshot.PropertyValues {
		if strings.EqualFold(registered.Property, property) &&
			strings.EqualFold(registered.Value, value) {
			return true
		}
	}
	return false
}

// IsRegisteredParameterValue reports case-insensitive registry membership.
func IsRegisteredParameterValue(
	snapshot registry.Snapshot,
	property string,
	parameter string,
	value string,
) bool {
	for _, registered := range snapshot.ParameterValues {
		if !strings.EqualFold(registered.Parameter, parameter) ||
			!strings.EqualFold(registered.Value, value) {
			continue
		}
		for _, applicableProperty := range registered.Properties {
			if strings.EqualFold(applicableProperty, property) {
				return true
			}
		}
	}
	return false
}
