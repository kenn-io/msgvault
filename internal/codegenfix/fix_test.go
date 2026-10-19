package codegenfix

import (
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
	assertions.NotContains(string(got), "typesValidator.Var(e.Excerpt")
	assertions.Contains(string(got), "JSON jsontext.Value")
	assertions.Contains(string(got), dailyNoteDecoyValidatorBlock())
	assertions.Contains(string(got), dailyNotePersonIDsValidatorBlock("gte=1"))
	assertions.NotContains(string(got), dailyNotePersonIDsValidatorBlock("omitempty,gte=1"))
}

func TestRewriteGeneratedValidatorsUsesOnlyPopulatedInboxStateBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	got, err := RewriteGeneratedValidators([]byte(
		generatedValidatorFixture() + generatedInboxStateValidatorFixture() + generatedOptionalValueJSONTagFixture(),
	))
	requirements.NoError(err)
	assertions.Contains(string(got), "sourcePopulated := i.Source != nil && *i.Source != (InboxSourceIdentity{})")
	assertions.Contains(string(got), "targetPopulated := i.Target != nil && *i.Target != (InboxTarget{})")
	assertions.Contains(string(got), "if sourcePopulated == targetPopulated {")
	assertions.Contains(string(got), "if sourcePopulated {")
	assertions.Contains(string(got), "if targetPopulated {")
	assertions.NotContains(string(got), "if i.Source != nil {\n\t\tif v, ok := any(i.Source).(runtime.Validator)")
	assertions.NotContains(string(got), "if i.Target != nil {\n\t\tif v, ok := any(i.Target).(runtime.Validator)")
	assertions.Contains(string(got), `json:"json,omitzero,omitempty"`)
	assertions.Contains(string(got), `json:"text,omitzero,omitempty"`)
	assertions.Contains(string(got), `json:"raw,omitzero,omitempty"`)
	again, err := RewriteGeneratedValidators(got)
	requirements.NoError(err)
	assertions.Equal(got, again)
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
type Evidence struct {
	Excerpt string
}
type ExploreCacheUnavailableResponse struct {
	RecoveryAction string ` + "`json:\"recovery_action\" validate:\"omitempty\"`" + `
}
func (e ExploreCacheUnavailableResponse) Validate() error {
	var errors runtime.ValidationErrors
` + exploreCacheRecoveryActionRequiredValidatorBlock() + `
}
func (e Evidence) Validate() error {
	var errors runtime.ValidationErrors
	if err := typesValidator.Var(e.Excerpt, "required"); err != nil {
		errors = errors.Append("Excerpt", err)
	}
	return errors
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
`
}

func generatedInboxStateValidatorFixture() string {
	return `
type InboxSourceIdentity struct { SourceID int64 }
type InboxTarget struct { SourceID int64; UID *int64 }
type InboxState struct { Source *InboxSourceIdentity; Target *InboxTarget }
func (i InboxState) Validate() error {
	var errors runtime.ValidationErrors
	if i.Source != nil {
		if v, ok := any(i.Source).(runtime.Validator); ok {
			if err := v.Validate(); err != nil {
				errors = errors.Append("Source", err)
			}
		}
	}
	if i.Target != nil {
		if v, ok := any(i.Target).(runtime.Validator); ok {
			if err := v.Validate(); err != nil {
				errors = errors.Append("Target", err)
			}
		}
	}
	if len(errors) == 0 { return nil }
	return errors
}
`
}

func generatedOptionalValueJSONTagFixture() string {
	return `type OptionalValueJSONTagFixture struct {
	Text *string ` + "`json:\"text,omitempty\"`" + `
	Raw jsontext.Value ` + "`json:\"raw,omitempty\"`" + `
}
`
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
