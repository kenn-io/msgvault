package codegenfix

import (
	"bytes"
	"errors"
	"fmt"
)

var generatedRequiredEmptyStringFields = [][2]string{
	{"PersonCardBriefText", "Text"},
	{"PersonCardCandidateText", "DisplayName"},
	{"PersonCardCite", "WebURL"},
	{"PersonCardItemText", "Text"},
	{"PersonCardLastInteraction", "Channel"},
	{"PersonCardLastInteraction", "Ref"},
	{"PersonCardLastInteraction", "WebURL"},
	{"PersonCardMeetingText", "Title"},
	{"PersonCardPerson", "InferredChannel"},
	{"PersonCardPerson", "PrimaryChannel"},
	{"PersonCardPerson", "VcardUID"},
	{"PersonCardPersonText", "DisplayName"},
	{"PersonCardPersonText", "OrganizationName"},
	{"PersonCardPersonText", "Title"},
	{"PersonCardSourceFreshness", "SyncCommand"},
	{"PersonCardSourceFreshness", "SyncUnavailableReason"},
	{"MeetingItem", "Title"},
	{"MeetingRef", "Title"},
	{"MeetingRecording", "MimeType"},
	{"MeetingRecordSummary", "Title"},
}

var generatedRequiredZeroTimeFields = [][2]string{
	{"PersonCardLastInteraction", "ProjectionComputedAt"},
	{"MeetingItem", "SentAt"},
}

func rewriteRequiredStringFieldPointer(source []byte, typeName, fieldName string) ([]byte, error) {
	return rewriteRequiredFieldPointer(source, typeName, fieldName, "string")
}

func rewriteRequiredTimeFieldPointer(source []byte, typeName, fieldName string) ([]byte, error) {
	return rewriteRequiredFieldPointer(source, typeName, fieldName, "time.Time")
}

func rewriteRequiredFieldPointer(source []byte, typeName, fieldName, fieldType string) ([]byte, error) {
	marker := []byte("type " + typeName + " struct {")
	start := bytes.Index(source, marker)
	if start < 0 {
		return nil, fmt.Errorf("generated %s.%s field changed", typeName, fieldName)
	}
	endOffset := bytes.Index(source[start:], []byte("\n}\n"))
	if endOffset < 0 {
		return nil, fmt.Errorf("generated %s.%s field changed", typeName, fieldName)
	}
	end := start + endOffset + 2
	block := bytes.Split(source[start:end], []byte("\n"))
	found := false
	for i := 1; i < len(block); i++ {
		fields := bytes.Fields(block[i])
		if len(fields) == 0 || !bytes.Equal(fields[0], []byte(fieldName)) {
			continue
		}
		if found || len(fields) < 2 || !bytes.Contains(block[i], []byte(`validate:"required"`)) {
			return nil, fmt.Errorf("generated %s.%s field changed", typeName, fieldName)
		}
		found = true
		switch string(fields[1]) {
		case fieldType:
			fieldStart := bytes.Index(block[i], fields[0]) + len(fields[0])
			typeOffset := bytes.Index(block[i][fieldStart:], fields[1]) + fieldStart
			block[i] = append(append(append([]byte(nil), block[i][:typeOffset]...), '*'), block[i][typeOffset:]...)
		case "*" + fieldType:
		default:
			return nil, fmt.Errorf("generated %s.%s field changed", typeName, fieldName)
		}
	}
	if !found {
		return nil, fmt.Errorf("generated %s.%s field changed", typeName, fieldName)
	}
	rewritten := bytes.Join(block, []byte("\n"))
	result := append([]byte(nil), source[:start]...)
	result = append(result, rewritten...)
	result = append(result, source[end:]...)
	return result, nil
}

// RewriteGeneratedRequiredFieldPointers preserves required fields that may
// carry empty strings or zero timestamps while keeping presence validation.
func RewriteGeneratedRequiredFieldPointers(source []byte) ([]byte, error) {
	result := append([]byte(nil), source...)
	for _, field := range generatedRequiredEmptyStringFields {
		if !bytes.Contains(result, []byte("type "+field[0]+" struct {")) {
			continue
		}
		var err error
		result, err = rewriteRequiredStringFieldPointer(result, field[0], field[1])
		if err != nil {
			return nil, err
		}
	}
	for _, field := range generatedRequiredZeroTimeFields {
		if !bytes.Contains(result, []byte("type "+field[0]+" struct {")) {
			continue
		}
		var err error
		result, err = rewriteRequiredTimeFieldPointer(result, field[0], field[1])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// RewritePersonCardNulls preserves required person-card values that may be
// empty or zero. It also repairs nullable object and map validation that the
// generator cannot express correctly on the generated Go types.
func RewritePersonCardNulls(source []byte) ([]byte, error) {
	if !bytes.Contains(source, []byte("type PersonCardResult struct {")) {
		return source, nil
	}
	result := source
	for name, fields := range map[string][]string{
		"PersonCardResult": {"card"},
		"PersonCard":       {"brief", "last_interaction"},
		"PersonCardBrief":  {"untrusted_text"},
	} {
		start := bytes.Index(result, []byte("type "+name+" struct {"))
		if start < 0 {
			return nil, fmt.Errorf("generated %s nullable fields changed", name)
		}
		endOffset := bytes.Index(result[start:], []byte("\n}\n"))
		if endOffset < 0 {
			return nil, fmt.Errorf("generated %s nullable fields changed", name)
		}
		end := start + endOffset
		block := append([]byte(nil), result[start:end]...)
		for _, field := range fields {
			oldTag := []byte(`json:"` + field + `,omitempty"`)
			newTag := []byte(`json:"` + field + `"`)
			if bytes.Count(block, oldTag)+bytes.Count(block, newTag) != 1 {
				return nil, fmt.Errorf("generated %s.%s nullable tag changed", name, field)
			}
			block = bytes.Replace(block, oldTag, newTag, 1)
		}
		result = append(append(append([]byte(nil), result[:start]...), block...), result[end:]...)
	}
	start := bytes.Index(result, []byte("func (p PersonCardFreshnessBlock) Validate() error {"))
	if start < 0 {
		return nil, errors.New("generated person card freshness validator changed")
	}
	endOffset := bytes.Index(result[start:], []byte("\n}\n"))
	if endOffset < 0 {
		return nil, errors.New("generated person card freshness validator changed")
	}
	end := start + endOffset
	block := result[start:end]
	loop := []byte("\tfor k, v := range p.Projections {\n")
	guarded := []byte("\tfor k, v := range p.Projections {\n\t\tif v == nil {\n\t\t\tcontinue\n\t\t}\n")
	if bytes.Count(block, loop) != 1 {
		return nil, errors.New("generated person card projection validator changed")
	}
	if !bytes.Contains(block, guarded) {
		block = bytes.Replace(block, loop, guarded, 1)
		result = append(append(append([]byte(nil), result[:start]...), block...), result[end:]...)
	}
	return result, nil
}
