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
var optionalSourceSliceJSONTag = regexp.MustCompile("(\\[\\]int64\\s+`json:\"source_ids?),omitempty(\")")

// RewriteSourceSelectionJSONTags keeps nil source filters omitted while
// preserving a non-nil empty slice as an explicit empty selection.
func RewriteSourceSelectionJSONTags(source []byte) ([]byte, error) {
	return optionalSourceSliceJSONTag.ReplaceAll(source, []byte("${1},omitzero${2}")), nil
}

// RewriteEmailFamilySourceSelectionQueries preserves an explicitly empty
// source selection in the generated query transport. The runtime's form-array
// encoder emits one parameter per value, so an empty slice would otherwise be
// omitted and interpreted as an unrestricted request.
func RewriteEmailFamilySourceSelectionQueries(source []byte) ([]byte, error) {
	for _, name := range []string{
		"GetEmailFamilyRequestOptions",
		"GetEmailFamilyDecisionRequestOptions",
		"ListEmailFamilyDecisionsRequestOptions",
	} {
		original := []byte(fmt.Sprintf("func (o *%s) GetQuery() (map[string]any, error) {\n\treturn runtime.AsMap[any](o.Query)\n}", name))
		replacement := []byte(fmt.Sprintf(`func (o *%s) GetQuery() (map[string]any, error) {
	params, err := runtime.AsMap[any](o.Query)
	if err != nil {
		return nil, err
	}
	if o.Query != nil && o.Query.SourceID != nil && len(o.Query.SourceID) == 0 {
		if params == nil {
			params = make(map[string]any, 1)
		}
		params["source_id"] = ""
	}
	return params, nil
}`, name))
		if bytes.Contains(source, replacement) {
			continue
		}
		if !bytes.Contains(source, original) {
			return nil, fmt.Errorf("generated %s GetQuery shape changed", name)
		}
		source = bytes.Replace(source, original, replacement, 1)
	}
	return source, nil
}

var requiredPointerValidators = [][3]string{
	{"CliAccountResponse", "Alias", "c"},
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
		{"EmailFamilyMember", "AccountAddress", "e"},
		{"EmailFamilyMember", "RawSha256", "e"},
		{"EmailFamilyMember", "Snippet", "e"},
		{"EmailFamilyMember", "Subject", "e"},
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
	memberStart := bytes.Index(result, []byte("type EmailFamilyMember struct {"))
	if memberStart < 0 {
		return nil, errors.New("generated EmailFamilyMember.OccurredAt field shape changed")
	}
	memberEndOffset := bytes.Index(result[memberStart:], []byte("\n}"))
	if memberEndOffset < 0 {
		return nil, errors.New("generated EmailFamilyMember.OccurredAt field shape changed")
	}
	if !regexp.MustCompile(`\bOccurredAt\s+time\.Time\b`).Match(result[memberStart : memberStart+memberEndOffset]) {
		return nil, errors.New("generated EmailFamilyMember.OccurredAt field shape changed")
	}
	validatorStart := bytes.Index(result, []byte("func (e EmailFamilyMember) Validate() error {"))
	if validatorStart < 0 {
		return nil, errors.New("generated EmailFamilyMember.OccurredAt validator shape changed")
	}
	validatorEndOffset := bytes.Index(result[validatorStart:], []byte("\n}\n"))
	if validatorEndOffset < 0 {
		return nil, errors.New("generated EmailFamilyMember.OccurredAt validator shape changed")
	}
	validatorEnd := validatorStart + validatorEndOffset
	occurredAtValidator := result[validatorStart:validatorEnd]
	requiredOccurredAt := []byte("\tif err := typesValidator.Var(e.OccurredAt, \"required\"); err != nil {\n\t\terrors = errors.Append(\"OccurredAt\", err)\n\t}\n")
	switch bytes.Count(occurredAtValidator, requiredOccurredAt) {
	case 1:
		rewritten := bytes.Replace(occurredAtValidator, requiredOccurredAt, nil, 1)
		result = append(append(append([]byte(nil), result[:validatorStart]...), rewritten...), result[validatorEnd:]...)
	case 0:
	default:
		return nil, errors.New("generated EmailFamilyMember.OccurredAt validator shape changed")
	}
	decisionStart := bytes.Index(result, []byte("type EmailFamilyDecision struct {"))
	if decisionStart < 0 {
		return nil, errors.New("generated EmailFamilyDecision.ActorRef field shape changed")
	}
	decisionEndOffset := bytes.Index(result[decisionStart:], []byte("\n}"))
	if decisionEndOffset < 0 {
		return nil, errors.New("generated EmailFamilyDecision.ActorRef field shape changed")
	}
	decisionEnd := decisionStart + decisionEndOffset
	decisionFields := result[decisionStart:decisionEnd]
	requiredActorRef := []byte("`json:\"actor_ref\" validate:\"required\"`")
	emptyActorRef := []byte("`json:\"actor_ref\"`")
	switch {
	case bytes.Count(decisionFields, requiredActorRef) == 1 && bytes.Count(decisionFields, emptyActorRef) == 0:
		fixed := bytes.Replace(decisionFields, requiredActorRef, emptyActorRef, 1)
		result = append(append(append([]byte(nil), result[:decisionStart]...), fixed...), result[decisionEnd:]...)
	case bytes.Count(decisionFields, requiredActorRef) == 0 && bytes.Count(decisionFields, emptyActorRef) == 1:
	default:
		return nil, errors.New("generated EmailFamilyDecision.ActorRef field shape changed")
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
	return RewriteSourceSelectionJSONTags(result)
}
