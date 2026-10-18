package codegenfix

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
)

// RewriteRunQueryClient preserves the simple generated SQL query methods' 200
// response type when the endpoint also declares a 202 accepted-build body.
// The WithResponse methods still expose both statuses to callers that need
// asynchronous refresh handling.
func RewriteRunQueryClient(source []byte) ([]byte, error) {
	for _, name := range []string{"RunQuery", "RunArchiveQuery"} {
		acceptedType := []byte(name + "ResponseJSON")
		rowsType := []byte(name + "Response")
		interfaceSignature := []byte(name + "(ctx context.Context, options *" + name + "RequestOptions, reqEditors ...runtime.RequestEditorFn) (*" + name + "ResponseJSON, error)")
		if !bytes.Contains(source, interfaceSignature) {
			return nil, fmt.Errorf("generated %s interface shape changed", name)
		}
		source = bytes.Replace(source, interfaceSignature, bytes.ReplaceAll(interfaceSignature, acceptedType, rowsType), 1)
		start := bytes.Index(source, []byte("func (c *Client) "+name+"("))
		if start < 0 {
			return nil, fmt.Errorf("generated %s method shape changed", name)
		}
		endOffset := bytes.Index(source[start:], []byte("\n}\n"))
		if endOffset < 0 {
			return nil, fmt.Errorf("generated %s method boundary changed", name)
		}
		end := start + endOffset
		method := append([]byte(nil), source[start:end]...)
		if !bytes.Contains(method, []byte("resp.StatusCode != 202")) || !bytes.Contains(method, acceptedType) {
			return nil, fmt.Errorf("generated %s success response shape changed", name)
		}
		method = bytes.ReplaceAll(method, acceptedType, rowsType)
		method = bytes.Replace(method, []byte("resp.StatusCode != 202"), []byte("resp.StatusCode != 200"), 1)
		result := append([]byte(nil), source[:start]...)
		result = append(result, method...)
		source = append(result, source[end:]...)
	}
	return source, nil
}

// Present empty strings and raw JSON values must survive JSON v2 encoding of generated responses.
var optionalValueJSONTag = regexp.MustCompile("((?:\\*string|\\*?jsontext\\.Value)\\s+`json:\"[^\"]+),omitempty(\")")

var requiredPointerValidators = [][3]string{
	{"InboxContext", "Text", "i"},
	{"FileMetadataResponse", "Filename", "f"},
	{"FileMetadataResponse", "MimeType", "f"},
	{"FileSearchRow", "Filename", "f"},
	{"FileSearchRow", "MimeType", "f"},
	{"PersonFileSearchRow", "Filename", "p"},
	{"PersonFileSearchRow", "MimeType", "p"},
	{"MessageRecording", "Filename", "m"},
	{"PersonFactEvidence", "SourceRef", "p"},
	{"PersonFactEvidence", "SourceURL", "p"},
	{"PersonFactEvidence", "ContentSha256", "p"},
	{"PersonFactEvidence", "SourceVersion", "p"},
	{"PersonFactEvidence", "SubjectRef", "p"},
	{"PersonFactEvidence", "Excerpt", "p"},
}

// RewriteGeneratedValidators preserves required-but-empty string fields as
// pointers while making their generated presence validation unconditional.
func RewriteGeneratedValidators(source []byte) ([]byte, error) {
	result := append([]byte(nil), source...)
	for _, target := range [][2]string{{"ExploreGroupsHTTPRequest", "e"}, {"FileGroupsHTTPRequest", "f"}} {
		typeName, receiver := target[0], target[1]
		marker := []byte("func (" + receiver + " " + typeName + ") Validate() error {\n\tvar errors runtime.ValidationErrors\n")
		validation := []byte("\tif err := typesValidator.Var(" + receiver + ".Grouping, \"required,min=1,max=1\"); err != nil {\n\t\terrors = errors.Append(\"Grouping\", err)\n\t}\n")
		start := bytes.Index(result, marker)
		if start < 0 {
			return nil, fmt.Errorf("generated %s validator shape changed", typeName)
		}
		endOffset := bytes.Index(result[start:], []byte("\n}\n"))
		if endOffset < 0 {
			return nil, fmt.Errorf("generated %s validator shape changed", typeName)
		}
		if validator := result[start : start+endOffset]; !bytes.Contains(validator, validation) {
			insertAt := start + len(marker)
			result = append(append(append([]byte(nil), result[:insertAt]...), validation...), result[insertAt:]...)
		}
	}
	for _, target := range requiredPointerValidators {
		typeName, field, receiver := target[0], target[1], target[2]
		startMarker := []byte("func (" + receiver + " " + typeName + ") Validate() error {")
		start := bytes.Index(result, startMarker)
		if start < 0 {
			return nil, fmt.Errorf("generated %s.%s validator shape changed", typeName, field)
		}
		endOffset := bytes.Index(result[start:], []byte("\n}\n"))
		if endOffset < 0 {
			return nil, fmt.Errorf("generated %s.%s validator shape changed", typeName, field)
		}
		end := start + endOffset
		validator := result[start:end]
		guarded := []byte("\tif " + receiver + "." + field + " != nil {\n\t\tif err := typesValidator.Var(" + receiver + "." + field + ", \"required\"); err != nil {\n\t\t\terrors = errors.Append(\"" + field + "\", err)\n\t\t}\n\t}")
		customType := []byte("\tif v, ok := any(" + receiver + "." + field + ").(runtime.Validator); ok {\n\t\tif err := v.Validate(); err != nil {\n\t\t\terrors = errors.Append(\"" + field + "\", err)\n\t\t}\n\t}")
		required := []byte("\tif err := typesValidator.Var(" + receiver + "." + field + ", \"required\"); err != nil {\n\t\terrors = errors.Append(\"" + field + "\", err)\n\t}")
		switch {
		case bytes.Contains(validator, guarded):
			rewritten := bytes.Replace(validator, guarded, required, 1)
			result = append(append(append([]byte(nil), result[:start]...), rewritten...), result[end:]...)
		case typeName == "InboxContext" && field == "Text" && bytes.Contains(validator, customType):
			// A plain *string cannot implement runtime.Validator. Presence is
			// mandatory, while a present empty archived body remains valid.
			rewritten := bytes.Replace(validator, customType, required, 1)
			result = append(append(append([]byte(nil), result[:start]...), rewritten...), result[end:]...)
		case bytes.Contains(validator, required):
		default:
			return nil, fmt.Errorf("generated %s.%s validator shape changed", typeName, field)
		}
	}
	// Required strings a response may send empty keep presence but lose their non-empty check.
	for _, target := range [][3]string{{"ExploreCacheUnavailableResponse", "RecoveryAction", "e"}, {"Evidence", "Excerpt", "e"}} {
		typeName, field, receiver := target[0], target[1], target[2]
		shapeErr := fmt.Errorf("generated %s.%s validator shape changed", typeName, field)
		start := bytes.Index(result, []byte("func ("+receiver+" "+typeName+") Validate() error {"))
		if start < 0 {
			return nil, shapeErr
		}
		endOffset := bytes.Index(result[start:], []byte("\n}\n"))
		if endOffset < 0 {
			return nil, shapeErr
		}
		end := start + endOffset
		validator := result[start:end]
		required := []byte("\tif err := typesValidator.Var(" + receiver + "." + field + ", \"required\"); err != nil {\n\t\terrors = errors.Append(\"" + field + "\", err)\n\t}\n")
		switch bytes.Count(validator, required) {
		case 1:
			rewritten := bytes.Replace(validator, required, nil, 1)
			result = append(append(append([]byte(nil), result[:start]...), rewritten...), result[end:]...)
		case 0:
			if !hasGeneratedStringField(result, typeName, field) {
				return nil, shapeErr
			}
		default:
			return nil, shapeErr
		}
	}
	const dailyNoteRequest = "CreateDailyNoteEntryRequest"
	startMarker := []byte("func (c " + dailyNoteRequest + ") Validate() error {")
	start := bytes.Index(result, startMarker)
	if start < 0 {
		return nil, errors.New("generated CreateDailyNoteEntryRequest.PersonIds validator shape changed")
	}
	endOffset := bytes.Index(result[start:], []byte("\n}\n"))
	if endOffset < 0 {
		return nil, errors.New("generated CreateDailyNoteEntryRequest.PersonIds validator shape changed")
	}
	end := start + endOffset
	validator := result[start:end]
	generatedPersonIDValidation := []byte(`	for i, item := range c.PersonIds {
		if err := typesValidator.Var(item, "omitempty,gte=1"); err != nil {
			errors = errors.Append(fmt.Sprintf("PersonIds[%d]", i), err)
		}
	}
`)
	positivePersonIDValidation := []byte(`	for i, item := range c.PersonIds {
		if err := typesValidator.Var(item, "gte=1"); err != nil {
			errors = errors.Append(fmt.Sprintf("PersonIds[%d]", i), err)
		}
	}
`)
	generatedCount := bytes.Count(validator, generatedPersonIDValidation)
	positiveCount := bytes.Count(validator, positivePersonIDValidation)
	switch {
	case generatedCount == 1 && positiveCount == 0:
		rewritten := bytes.Replace(validator, generatedPersonIDValidation, positivePersonIDValidation, 1)
		result = append(append(append([]byte(nil), result[:start]...), rewritten...), result[end:]...)
	case generatedCount == 0 && positiveCount == 1:
	default:
		return nil, errors.New("generated CreateDailyNoteEntryRequest.PersonIds validator shape changed")
	}
	attributeJSON := []byte("*struct{}  `json:\"json,omitempty\"`")
	if !bytes.Contains(result, attributeJSON) {
		return nil, errors.New("generated AttributeValue.JSON shape changed")
	}
	result = bytes.Replace(result, attributeJSON,
		[]byte("jsontext.Value `json:\"json,omitempty\"`"), 1)
	result = optionalValueJSONTag.ReplaceAll(result, []byte("${1},omitzero${2}"))
	result, err := rewriteInboxStateBindingValidator(result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func hasGeneratedStringField(source []byte, typeName, field string) bool {
	start := bytes.Index(source, []byte("type "+typeName+" struct {"))
	if start < 0 {
		return false
	}
	endOffset := bytes.Index(source[start:], []byte("\n}"))
	if endOffset < 0 {
		return false
	}
	fieldPattern := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(field) + `[ \t]+string\b`)
	return fieldPattern.Match(source[start : start+endOffset])
}

// InboxState is a union: item observations populate Target, while folder
// observations populate Source. The generated validator must validate only
// the populated binding, and reject ambiguous or empty bindings.
func rewriteInboxStateBindingValidator(source []byte) ([]byte, error) {
	const marker = "func (i InboxState) Validate() error {"
	if !bytes.Contains(source, []byte(marker)) {
		return source, nil
	}
	const generated = `	if v, ok := any(i.Source).(runtime.Validator); ok {
		if err := v.Validate(); err != nil {
			errors = errors.Append("Source", err)
		}
	}
	if v, ok := any(i.Target).(runtime.Validator); ok {
		if err := v.Validate(); err != nil {
			errors = errors.Append("Target", err)
		}
	}`
	const conditional = `	sourcePopulated := i.Source.AccountID != "" || i.Source.SourceID != 0 || i.Source.SourceIdentifier != "" || i.Source.SourceType != ""
	targetPopulated := i.Target.AccountID != "" || i.Target.ItemID != 0 || i.Target.ProviderID != "" || i.Target.Scope != "" || i.Target.SourceID != 0 || i.Target.SourceIdentifier != "" || i.Target.SourceType != "" || i.Target.Mailbox != nil || i.Target.UID != nil || i.Target.Uidvalidity != nil
	if sourcePopulated == targetPopulated {
		errors = errors.Append("Binding", fmt.Errorf("exactly one of source or target must be populated"))
	} else if sourcePopulated {
		if err := i.Source.Validate(); err != nil {
			errors = errors.Append("Source", err)
		}
	} else if err := i.Target.Validate(); err != nil {
		errors = errors.Append("Target", err)
	}`
	generatedCount := bytes.Count(source, []byte(generated))
	conditionalCount := bytes.Count(source, []byte(conditional))
	switch {
	case generatedCount == 1 && conditionalCount == 0:
		return bytes.Replace(source, []byte(generated), []byte(conditional), 1), nil
	case generatedCount == 0 && conditionalCount == 1:
		return source, nil
	default:
		return nil, errors.New("generated InboxState binding validator shape changed")
	}
}
