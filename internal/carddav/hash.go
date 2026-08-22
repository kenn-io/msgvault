package carddav

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/vcard"
)

var serverOwnedProperties = map[string]bool{
	"PRODID": true, "REV": true, "SOURCE": true,
	"CREATED": true, "LAST-MODIFIED": true,
}

type semanticProperty struct {
	Group      string              `json:"group,omitempty"`
	Name       string              `json:"name"`
	Parameters []semanticParameter `json:"parameters,omitempty"`
	ValueType  string              `json:"value_type,omitempty"`
	RawValue   string              `json:"raw_value"`
}

type semanticParameter struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// SemanticHash hashes the parsed vCard rather than its wire formatting. The
// five properties CardDAV servers conventionally own are deliberately absent,
// so their churn cannot masquerade as a user edit.
func SemanticHash(body []byte) (string, error) {
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return "", fmt.Errorf("parse vCard for semantic hash: %w", err)
	}
	properties := make([]semanticProperty, 0, len(envelope.PropertyTree))
	for _, occurrence := range envelope.PropertyTree {
		property := occurrence.Property
		name := strings.ToUpper(property.Name)
		if serverOwnedProperties[name] {
			continue
		}
		parameters := make([]semanticParameter, 0, len(property.Parameters))
		for _, parameter := range property.Parameters {
			if strings.EqualFold(parameter.Name, "VALUE") {
				continue
			}
			values := make([]string, 0, len(parameter.Values))
			for _, value := range parameter.Values {
				decoded := value.Decoded
				if parameter.Name == "TYPE" || parameter.Name == "ENCODING" ||
					parameter.Name == "VALUE" || parameter.Name == "MEDIATYPE" {
					decoded = strings.ToLower(decoded)
				}
				values = append(values, decoded)
			}
			if strings.EqualFold(parameter.Name, "TYPE") {
				slices.Sort(values)
				values = slices.Compact(values)
			}
			parameters = append(parameters, semanticParameter{
				Name: strings.ToUpper(parameter.Name), Values: values,
			})
		}
		slices.SortFunc(parameters, func(left, right semanticParameter) int {
			return strings.Compare(left.Name, right.Name)
		})
		properties = append(properties, semanticProperty{
			Group: strings.ToLower(property.Group), Name: name,
			Parameters: parameters,
			ValueType:  semanticValueType(envelope.RenderMetadata.StoredVersion, property),
			RawValue:   canonicalSemanticValue(envelope.RenderMetadata.StoredVersion, property),
		})
	}
	slices.SortFunc(properties, compareSemanticProperties)
	encoded, err := json.Marshal(properties)
	if err != nil {
		return "", fmt.Errorf("encode semantic vCard: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func semanticValueType(version vcard.Version, property vcard.Property) string {
	for _, parameter := range property.Parameters {
		if strings.EqualFold(parameter.Name, "VALUE") && len(parameter.Values) > 0 {
			return strings.ToLower(strings.TrimSpace(parameter.Values[0].Decoded))
		}
	}
	name := strings.ToUpper(property.Name)
	if version == vcard.Version40 && v4URIProperties[name] {
		return "uri"
	}
	if legacyURIProperties[name] {
		return "uri"
	}
	if textProperties[name] {
		return "text"
	}
	return ""
}

func canonicalSemanticValue(version vcard.Version, property vcard.Property) string {
	value := strings.ReplaceAll(strings.ReplaceAll(property.RawValue, "\r\n", "\n"), "\r", "\n")
	switch semanticValueType(version, property) {
	case "text":
		return canonicalTextEscapes(value)
	case "uri":
		return canonicalURI(value)
	case "boolean", "language-tag":
		return strings.ToLower(value)
	default:
		return value
	}
}

// canonicalTextEscapes preserves structured and list delimiters while making
// the two RFC 6350 spellings of an escaped newline identical.
func canonicalTextEscapes(value string) string {
	var canonical strings.Builder
	canonical.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+1 < len(value) && value[index+1] == 'N' {
			canonical.WriteString(`\n`)
			index++
			continue
		}
		canonical.WriteByte(value[index])
	}
	return canonical.String()
}

func canonicalURI(value string) string {
	if !vcard.IsURIValue(value) {
		return value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	return parsed.String()
}

var textProperties = map[string]bool{
	"VERSION": true, "KIND": true, "XML": true, "FN": true, "N": true,
	"NICKNAME": true, "GENDER": true, "ADR": true, "EMAIL": true,
	"TITLE": true, "ROLE": true, "ORG": true, "CATEGORIES": true,
	"NOTE": true, "PRODID": true, "CLIENTPIDMAP": true,
}

var legacyURIProperties = map[string]bool{
	"SOURCE": true, "URL": true, "IMPP": true,
}

var v4URIProperties = map[string]bool{
	"SOURCE": true, "PHOTO": true, "TEL": true, "IMPP": true, "GEO": true,
	"LOGO": true, "MEMBER": true, "RELATED": true, "SOUND": true,
	"UID": true, "URL": true, "KEY": true, "FBURL": true,
	"CALADRURI": true, "CALURI": true, "BIRTHPLACE": true,
	"DEATHPLACE": true, "CONTACT-URI": true, "ORG-DIRECTORY": true,
}

func compareSemanticProperties(left, right semanticProperty) int {
	if result := cmp.Compare(left.Group, right.Group); result != 0 {
		return result
	}
	if result := cmp.Compare(left.Name, right.Name); result != 0 {
		return result
	}
	if result := slices.CompareFunc(left.Parameters, right.Parameters, func(
		left, right semanticParameter,
	) int {
		if result := cmp.Compare(left.Name, right.Name); result != 0 {
			return result
		}
		return slices.Compare(left.Values, right.Values)
	}); result != 0 {
		return result
	}
	if result := cmp.Compare(left.ValueType, right.ValueType); result != 0 {
		return result
	}
	return cmp.Compare(left.RawValue, right.RawValue)
}
