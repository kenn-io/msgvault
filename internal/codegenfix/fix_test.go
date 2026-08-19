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
	assertions.Contains(string(got), `typesValidator.Var(e.Grouping, "required,min=1,max=1")`)
	assertions.Contains(string(got), `typesValidator.Var(f.Grouping, "required,min=1,max=1")`)
	assertions.NotContains(string(got), exploreCacheRecoveryActionRequiredValidatorBlock())
	assertions.Contains(string(got), "JSON json.RawMessage")
	assertions.Contains(string(got), dailyNoteDecoyValidatorBlock())
	assertions.Contains(string(got), dailyNotePersonIDsValidatorBlock("gte=1"))
	assertions.NotContains(string(got), dailyNotePersonIDsValidatorBlock("omitempty,gte=1"))
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
		pointerValidatorFixture("PersonFileSearchRow", "p") + `
func (c CreateDailyNoteEntryRequest) Validate() error {
	var errors runtime.ValidationErrors
` + dailyNoteDecoyValidatorBlock() + dailyNotePersonIDsValidatorBlock("omitempty,gte=1") + `
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
