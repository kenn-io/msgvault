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
		fixedInterfaceSignature := bytes.ReplaceAll(interfaceSignature, acceptedType, rowsType)
		rawInterfaceIndex := bytes.Index(source, interfaceSignature)
		fixedInterfaceIndex := bytes.Index(source, fixedInterfaceSignature)
		switch {
		case rawInterfaceIndex >= 0 && (fixedInterfaceIndex < 0 || rawInterfaceIndex < fixedInterfaceIndex):
			source = bytes.Replace(source, interfaceSignature, fixedInterfaceSignature, 1)
		case fixedInterfaceIndex >= 0 && (rawInterfaceIndex < 0 || fixedInterfaceIndex < rawInterfaceIndex):
		default:
			return nil, fmt.Errorf("generated %s interface shape changed", name)
		}
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
		rawResponse := bytes.Contains(method, []byte("resp.StatusCode != 202")) && bytes.Contains(method, acceptedType)
		fixedResponse := !bytes.Contains(method, []byte("resp.StatusCode != 202")) &&
			bytes.Contains(method, []byte("resp.StatusCode != 200")) && bytes.Contains(method, rowsType)
		switch {
		case rawResponse && !fixedResponse:
			method = bytes.ReplaceAll(method, acceptedType, rowsType)
			method = bytes.Replace(method, []byte("resp.StatusCode != 202"), []byte("resp.StatusCode != 200"), 1)
		case fixedResponse && !rawResponse:
		default:
			return nil, fmt.Errorf("generated %s success response shape changed", name)
		}
		result := append([]byte(nil), source[:start]...)
		result = append(result, method...)
		source = append(result, source[end:]...)
	}
	return source, nil
}

// Present empty strings and raw JSON values must survive JSON v2 encoding of generated responses.
var optionalValueJSONTag = regexp.MustCompile("((?:\\*string|\\*?jsontext\\.Value)\\s+`json:\"[^\"]+),omitempty(\")")

var requiredPointerValidators = [][3]string{
	{"FileMetadataResponse", "Filename", "f"},
	{"FileMetadataResponse", "MimeType", "f"},
	{"FileSearchRow", "Filename", "f"},
	{"FileSearchRow", "MimeType", "f"},
	{"PersonFileSearchRow", "Filename", "p"},
	{"PersonFileSearchRow", "MimeType", "p"},
	{"FileSuggestionRow", "MimeType", "f"},
	{"PersonFileSuggestionRow", "MimeType", "p"},
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
		validatorEnd := start + endOffset + 1
		switch bytes.Count(result[start:validatorEnd], validation) {
		case 0:
			insertAt := start + len(marker)
			result = append(append(append([]byte(nil), result[:insertAt]...), validation...), result[insertAt:]...)
		case 1:
		default:
			return nil, fmt.Errorf("generated %s validator has duplicate grouping checks", typeName)
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
		required := []byte("\tif err := typesValidator.Var(" + receiver + "." + field + ", \"required\"); err != nil {\n\t\terrors = errors.Append(\"" + field + "\", err)\n\t}")
		switch {
		case bytes.Contains(validator, guarded):
			rewritten := bytes.Replace(validator, guarded, required, 1)
			result = append(append(append([]byte(nil), result[:start]...), rewritten...), result[end:]...)
		case bytes.Contains(validator, required):
		default:
			return nil, fmt.Errorf("generated %s.%s validator shape changed", typeName, field)
		}
	}
	// Required strings a response may send empty keep presence but lose their non-empty check.
	for _, target := range [][3]string{
		{"ExploreCacheUnavailableResponse", "RecoveryAction", "e"},
		{"Evidence", "Excerpt", "e"},
		{"ContactCandidate", "DisplayName", "c"},
		{"MessagingRoute", "MergedIntoChatID", "m"},
		{"MessagingRoute", "Network", "m"},
		{"MessagingRoute", "NetworkLabel", "m"},
		{"MessagingRoute", "ProviderChatID", "m"},
		{"PersonMessagingRoutesPage", "AliasReason", "p"},
	} {
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
			if !regexp.MustCompile(`\b` + field + `\s+string\b`).Match(result) {
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
	fixedAttributeJSON := []byte("jsontext.Value `json:\"json,omitzero\"`")
	switch {
	case bytes.Count(result, attributeJSON) == 1 && bytes.Count(result, fixedAttributeJSON) == 0:
		result = bytes.Replace(result, attributeJSON, fixedAttributeJSON, 1)
	case bytes.Count(result, attributeJSON) == 0 && bytes.Count(result, fixedAttributeJSON) == 1:
	default:
		return nil, errors.New("generated AttributeValue.JSON shape changed")
	}
	result = optionalValueJSONTag.ReplaceAll(result, []byte("${1},omitzero${2}"))
	return result, nil
}
