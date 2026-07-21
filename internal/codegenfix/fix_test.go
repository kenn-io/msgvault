package codegenfix

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteGeneratedValidatorsMakesRequiredPointersUnconditional(t *testing.T) {
	assertions := assert.New(t)
	got, err := RewriteGeneratedValidators([]byte(generatedValidatorFixture()))
	require.NoError(t, err)
	assertions.NotContains(string(got), "if f.Filename != nil")
	assertions.NotContains(string(got), "if f.MimeType != nil")
	assertions.Contains(string(got), `typesValidator.Var(e.Grouping, "required,min=1,max=1")`)
	assertions.Contains(string(got), `typesValidator.Var(f.Grouping, "required,min=1,max=1")`)
}

func TestRewriteGeneratedValidatorsRejectsMissingGroupingValidator(t *testing.T) {
	_, err := RewriteGeneratedValidators([]byte("package generated\n"))

	require.ErrorContains(t, err, "ExploreGroupsHTTPRequest validator shape changed")
}

func generatedValidatorFixture() string {
	return `func (e ExploreGroupsHTTPRequest) Validate() error {
	var errors runtime.ValidationErrors
}
func (f FileGroupsHTTPRequest) Validate() error {
	var errors runtime.ValidationErrors
}
` + pointerValidatorFixture("FileMetadataResponse") + pointerValidatorFixture("FileSearchRow")
}

func pointerValidatorFixture(typeName string) string {
	return `func (f ` + typeName + `) Validate() error {
	var errors runtime.ValidationErrors
	if f.Filename != nil {
		if err := typesValidator.Var(f.Filename, "required"); err != nil {
			errors = errors.Append("Filename", err)
		}
	}
	if f.MimeType != nil {
		if err := typesValidator.Var(f.MimeType, "required"); err != nil {
			errors = errors.Append("MimeType", err)
		}
	}
}
`
}
