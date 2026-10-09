package codegenfix

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteGeneratedValidatorsRepairsKnownGeneratorGaps(t *testing.T) {
	assertions := assert.New(t)
	got, err := RewriteGeneratedValidators([]byte(generatedValidatorFixture()))
	require.NoError(t, err)
	assertions.NotContains(string(got), "if f.Filename != nil")
	assertions.NotContains(string(got), "if f.MimeType != nil")
	assertions.NotContains(string(got), "if p.Filename != nil")
	assertions.NotContains(string(got), "if p.MimeType != nil")
	assertions.NotContains(string(got), "if p.SourceRef != nil")
	assertions.NotContains(string(got), "if m.Filename != nil")
	assertions.NotContains(string(got), "if p.SourceURL != nil")
	assertions.NotContains(string(got), "if p.ContentSha256 != nil")
	assertions.NotContains(string(got), "if p.SourceVersion != nil")
	assertions.NotContains(string(got), "if p.SubjectRef != nil")
	assertions.NotContains(string(got), "if p.Excerpt != nil")
	assertions.Contains(string(got), `typesValidator.Var(e.Grouping, "required,min=1,max=1")`)
	assertions.Contains(string(got), `typesValidator.Var(f.Grouping, "required,min=1,max=1")`)
	assertions.NotContains(string(got), exploreCacheRecoveryActionRequiredValidatorBlock())
	assertions.Contains(string(got), "JSON jsontext.Value")
	assertions.Contains(string(got), dailyNoteDecoyValidatorBlock())
	assertions.Contains(string(got), dailyNotePersonIDsValidatorBlock("gte=1"))
	assertions.NotContains(string(got), dailyNotePersonIDsValidatorBlock("omitempty,gte=1"))
	again, err := RewriteGeneratedValidators(got)
	require.NoError(t, err)
	if !bytes.Equal(got, again) {
		index := firstDifference(got, again)
		start := max(0, index-40)
		leftEnd, rightEnd := min(len(got), index+100), min(len(again), index+100)
		assertions.Failf("validator rewrite is not idempotent", "first difference at byte %d: got=%q again=%q", index, got[start:leftEnd], again[start:rightEnd])
	}
}

func firstDifference(left, right []byte) int {
	for i := range min(len(left), len(right)) {
		if left[i] != right[i] {
			return i
		}
	}
	return min(len(left), len(right))
}

func TestRewriteGeneratedValidatorsAllowsEmptyContactRouteStrings(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	got, err := RewriteGeneratedValidators([]byte(generatedValidatorFixture()))
	requirements.NoError(err)
	assertions.NotContains(string(got), `typesValidator.Var(m.MergedIntoChatID, "required")`)
	assertions.NotContains(string(got), `typesValidator.Var(m.Network, "required")`)
	assertions.NotContains(string(got), `typesValidator.Var(m.NetworkLabel, "required")`)
	assertions.NotContains(string(got), `typesValidator.Var(m.ProviderChatID, "required")`)
	assertions.NotContains(string(got), `typesValidator.Var(p.AliasReason, "required")`)
	assertions.NotContains(string(got), `typesValidator.Var(c.DisplayName, "required")`)
}

func TestRewriteRunQueryClientPreservesSynchronousResultType(t *testing.T) {
	assert := assert.New(t)
	methods := `type RunQueryClientInterface interface {
	RunQuery(ctx context.Context, options *RunQueryRequestOptions, reqEditors ...runtime.RequestEditorFn) (*RunQueryResponseJSON, error)
}
func (c *Client) RunQuery(ctx context.Context, options *RunQueryRequestOptions, reqEditors ...runtime.RequestEditorFn) (*RunQueryResponseJSON, error) {
	responseParser := func(ctx context.Context, resp *runtime.Response) (*RunQueryResponseJSON, error) {
		if resp.StatusCode != 202 { return nil, nil }
		target := new(RunQueryResponseJSON)
		return target, nil
	}
	return responseParser(ctx, nil)
}
`
	fixture := "package generated\ntype Client struct{}\n" + methods + strings.ReplaceAll(methods, "RunQuery", "RunArchiveQuery")
	got, err := RewriteRunQueryClient([]byte(fixture))
	require.NoError(t, err)
	assert.Equal(2, strings.Count(string(got), "resp.StatusCode != 200"))
	assert.NotContains(string(got), "RunQueryResponseJSON")
	assert.NotContains(string(got), "RunArchiveQueryResponseJSON")
	again, err := RewriteRunQueryClient(got)
	require.NoError(t, err)
	assert.Equal(got, again)
}

func TestRewriteGeneratedValidatorsRejectsMissingGroupingValidator(t *testing.T) {
	_, err := RewriteGeneratedValidators([]byte("package generated\n"))

	require.ErrorContains(t, err, "ExploreGroupsHTTPRequest validator shape changed")
}

func TestRewriteGeneratedValidatorsRejectsChangedDailyNotePersonIDValidator(t *testing.T) {
	oldBlock := dailyNotePersonIDsValidatorBlock("omitempty,gte=1")
	fixture := strings.Replace(
		generatedValidatorFixture(),
		oldBlock,
		strings.Replace(oldBlock, `"omitempty,gte=1"`, `"gt=0"`, 1),
		1,
	)

	_, err := RewriteGeneratedValidators([]byte(fixture))

	require.ErrorContains(t, err, "CreateDailyNoteEntryRequest.PersonIds validator shape changed")
}

func TestRewriteGeneratedValidatorsAcceptsOneFixedDailyNotePersonIDValidator(t *testing.T) {
	oldBlock := dailyNotePersonIDsValidatorBlock("omitempty,gte=1")
	fixedBlock := dailyNotePersonIDsValidatorBlock("gte=1")
	fixture := strings.Replace(generatedValidatorFixture(), oldBlock, fixedBlock, 1)

	got, err := RewriteGeneratedValidators([]byte(fixture))

	require.NoError(t, err)
	assert.Contains(t, string(got), fixedBlock)
}

func TestRewriteGeneratedValidatorsRejectsAmbiguousDailyNotePersonIDValidators(t *testing.T) {
	oldBlock := dailyNotePersonIDsValidatorBlock("omitempty,gte=1")
	fixedBlock := dailyNotePersonIDsValidatorBlock("gte=1")
	for _, test := range []struct {
		name        string
		replacement string
	}{
		{name: "two generated blocks", replacement: oldBlock + oldBlock},
		{name: "generated and fixed blocks", replacement: oldBlock + fixedBlock},
		{name: "two fixed blocks", replacement: fixedBlock + fixedBlock},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := strings.Replace(
				generatedValidatorFixture(),
				oldBlock,
				test.replacement,
				1,
			)

			_, err := RewriteGeneratedValidators([]byte(fixture))

			require.ErrorContains(t, err, "CreateDailyNoteEntryRequest.PersonIds validator shape changed")
		})
	}
}

func generatedValidatorFixture() string {
	return `type AttributeValue struct {
	JSON *struct{}  ` + "`json:\"json,omitempty\"`" + `
}
type ExploreCacheUnavailableResponse struct {
	RecoveryAction string ` + "`json:\"recovery_action\" validate:\"omitempty\"`" + `
}
func (e ExploreCacheUnavailableResponse) Validate() error {
	var errors runtime.ValidationErrors
` + exploreCacheRecoveryActionRequiredValidatorBlock() + `
}
func (e ExploreGroupsHTTPRequest) Validate() error {
	var errors runtime.ValidationErrors
}
func (f FileGroupsHTTPRequest) Validate() error {
	var errors runtime.ValidationErrors
}
` + pointerValidatorFixture("FileMetadataResponse", "f") + pointerValidatorFixture("FileSearchRow", "f") +
		pointerValidatorFixture("PersonFileSearchRow", "p") +
		requiredStringPointerValidatorFixture("MessageRecording", "m", "Filename") +
		requiredStringPointerValidatorFixture("PersonFactEvidence", "p",
			"SourceRef", "SourceURL", "ContentSha256", "SourceVersion", "SubjectRef", "Excerpt") + `
func (c CreateDailyNoteEntryRequest) Validate() error {
	var errors runtime.ValidationErrors
` + dailyNoteDecoyValidatorBlock() + dailyNotePersonIDsValidatorBlock("omitempty,gte=1") + `
}
` + contactRouteEmptyStringValidatorFixture() + contactCandidateEmptyStringValidatorFixture()
}

func contactRouteEmptyStringValidatorFixture() string {
	return "type MessagingRoute struct{}\n" +
		"func (m MessagingRoute) Validate() error {\n\tvar errors runtime.ValidationErrors\n" +
		"\tif err := typesValidator.Var(m.MergedIntoChatID, \"required\"); err != nil {\n\t\terrors = errors.Append(\"MergedIntoChatID\", err)\n\t}\n" +
		"\tif err := typesValidator.Var(m.Network, \"required\"); err != nil {\n\t\terrors = errors.Append(\"Network\", err)\n\t}\n" +
		"\tif err := typesValidator.Var(m.NetworkLabel, \"required\"); err != nil {\n\t\terrors = errors.Append(\"NetworkLabel\", err)\n\t}\n" +
		"\tif err := typesValidator.Var(m.ProviderChatID, \"required\"); err != nil {\n\t\terrors = errors.Append(\"ProviderChatID\", err)\n\t}\n}\n" +
		"type PersonMessagingRoutesPage struct{}\n" +
		"func (p PersonMessagingRoutesPage) Validate() error {\n\tvar errors runtime.ValidationErrors\n" +
		"\tif err := typesValidator.Var(p.AliasReason, \"required\"); err != nil {\n\t\terrors = errors.Append(\"AliasReason\", err)\n\t}\n}\n"
}

func contactCandidateEmptyStringValidatorFixture() string {
	return "type ContactCandidate struct{}\n" +
		"func (c ContactCandidate) Validate() error {\n\tvar errors runtime.ValidationErrors\n" +
		"\tif err := typesValidator.Var(c.DisplayName, \"required\"); err != nil {\n\t\terrors = errors.Append(\"DisplayName\", err)\n\t}\n" +
		"\tif len(errors) == 0 {\n\t\treturn nil\n\t}\n\treturn errors\n}\n"
}

func exploreCacheRecoveryActionRequiredValidatorBlock() string {
	return `	if err := typesValidator.Var(e.RecoveryAction, "required"); err != nil {
		errors = errors.Append("RecoveryAction", err)
	}
`
}

func dailyNoteDecoyValidatorBlock() string {
	return `	for i, item := range c.RelatedIds {
		if err := typesValidator.Var(item, "omitempty,gte=1"); err != nil {
			errors = errors.Append(fmt.Sprintf("RelatedIds[%d]", i), err)
		}
	}
`
}

func dailyNotePersonIDsValidatorBlock(tag string) string {
	return `	for i, item := range c.PersonIds {
		if err := typesValidator.Var(item, "` + tag + `"); err != nil {
			errors = errors.Append(fmt.Sprintf("PersonIds[%d]", i), err)
		}
	}
`
}

func pointerValidatorFixture(typeName, receiver string) string {
	return `func (` + receiver + ` ` + typeName + `) Validate() error {
	var errors runtime.ValidationErrors
	if ` + receiver + `.Filename != nil {
		if err := typesValidator.Var(` + receiver + `.Filename, "required"); err != nil {
			errors = errors.Append("Filename", err)
		}
	}
	if ` + receiver + `.MimeType != nil {
		if err := typesValidator.Var(` + receiver + `.MimeType, "required"); err != nil {
			errors = errors.Append("MimeType", err)
		}
	}
}
`
}

func requiredStringPointerValidatorFixture(
	typeName, receiver string, fields ...string,
) string {
	var result strings.Builder
	result.WriteString("func (")
	result.WriteString(receiver)
	result.WriteString(" ")
	result.WriteString(typeName)
	result.WriteString(") Validate() error {\n\tvar errors runtime.ValidationErrors\n")
	for _, field := range fields {
		result.WriteString("\tif ")
		result.WriteString(receiver)
		result.WriteString(".")
		result.WriteString(field)
		result.WriteString(" != nil {\n\t\tif err := typesValidator.Var(")
		result.WriteString(receiver)
		result.WriteString(".")
		result.WriteString(field)
		result.WriteString(", \"required\"); err != nil {\n\t\t\terrors = errors.Append(\"")
		result.WriteString(field)
		result.WriteString("\", err)\n\t\t}\n\t}\n")
	}
	result.WriteString("}\n")
	return result.String()
}
