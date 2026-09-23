package codegenfix

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
)

// RewriteRunQueryClient preserves the simple generated RunQuery method's 200
// response type when the endpoint also declares a 202 accepted-build body.
// The WithResponse method still exposes both statuses to callers that need
// asynchronous refresh handling.
func RewriteRunQueryClient(source []byte) ([]byte, error) {
	interfaceSignature := []byte("RunQuery(ctx context.Context, options *RunQueryRequestOptions, reqEditors ...runtime.RequestEditorFn) (*RunQueryResponseJSON, error)")
	if !bytes.Contains(source, interfaceSignature) {
		return nil, errors.New("generated RunQuery interface shape changed")
	}
	source = bytes.Replace(source, interfaceSignature,
		[]byte("RunQuery(ctx context.Context, options *RunQueryRequestOptions, reqEditors ...runtime.RequestEditorFn) (*RunQueryResponse, error)"), 1)
	start := bytes.Index(source, []byte("func (c *Client) RunQuery("))
	if start < 0 {
		return nil, errors.New("generated RunQuery method shape changed")
	}
	endOffset := bytes.Index(source[start:], []byte("\n// ListRelationshipTypes "))
	if endOffset < 0 {
		return nil, errors.New("generated RunQuery method boundary changed")
	}
	end := start + endOffset
	method := append([]byte(nil), source[start:end]...)
	if !bytes.Contains(method, []byte("resp.StatusCode != 202")) ||
		!bytes.Contains(method, []byte("RunQueryResponseJSON")) {
		return nil, errors.New("generated RunQuery success response shape changed")
	}
	method = bytes.ReplaceAll(method, []byte("RunQueryResponseJSON"), []byte("RunQueryResponse"))
	method = bytes.Replace(method, []byte("resp.StatusCode != 202"), []byte("resp.StatusCode != 200"), 1)
	result := append([]byte(nil), source[:start]...)
	result = append(result, method...)
	result = append(result, source[end:]...)
	return result, nil
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
	const cacheUnavailableType = "ExploreCacheUnavailableResponse"
	cacheValidatorStart := []byte("func (e " + cacheUnavailableType + ") Validate() error {")
	start := bytes.Index(result, cacheValidatorStart)
	if start < 0 {
		return nil, errors.New("generated ExploreCacheUnavailableResponse.RecoveryAction validator shape changed")
	}
	endOffset := bytes.Index(result[start:], []byte("\n}\n"))
	if endOffset < 0 {
		return nil, errors.New("generated ExploreCacheUnavailableResponse.RecoveryAction validator shape changed")
	}
	end := start + endOffset
	validator := result[start:end]
	requiredRecoveryAction := []byte("\tif err := typesValidator.Var(e.RecoveryAction, \"required\"); err != nil {\n\t\terrors = errors.Append(\"RecoveryAction\", err)\n\t}\n")
	switch bytes.Count(validator, requiredRecoveryAction) {
	case 1:
		rewritten := bytes.Replace(validator, requiredRecoveryAction, nil, 1)
		result = append(append(append([]byte(nil), result[:start]...), rewritten...), result[end:]...)
	case 0:
		if !bytes.Contains(result, []byte("RecoveryAction string")) {
			return nil, errors.New("generated ExploreCacheUnavailableResponse.RecoveryAction validator shape changed")
		}
	default:
		return nil, errors.New("generated ExploreCacheUnavailableResponse.RecoveryAction validator shape changed")
	}
	const dailyNoteRequest = "CreateDailyNoteEntryRequest"
	startMarker := []byte("func (c " + dailyNoteRequest + ") Validate() error {")
	start = bytes.Index(result, startMarker)
	if start < 0 {
		return nil, errors.New("generated CreateDailyNoteEntryRequest.PersonIds validator shape changed")
	}
	endOffset = bytes.Index(result[start:], []byte("\n}\n"))
	if endOffset < 0 {
		return nil, errors.New("generated CreateDailyNoteEntryRequest.PersonIds validator shape changed")
	}
	end = start + endOffset
	validator = result[start:end]
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
	return result, nil
}
