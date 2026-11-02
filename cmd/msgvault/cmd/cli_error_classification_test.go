package cmd

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"testing"
)

type testExitCauseError struct{ code int }

func (e testExitCauseError) Error() string { return "fixture child" }
func (e testExitCauseError) ExitCode() int { return e.code }

func TestClassificationPreservesAllCauses(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	original := errors.Join(usageErr(nil, first), second)
	classified := classifyCLIError(nil, original)
	require.ErrorIs(t, classified, first)
	require.ErrorIs(t, classified, second)
}
func TestClassificationPrecedence(t *testing.T) {
	for _, cause := range []error{testExitCauseError{4}, &daemonclient.APIError{Status: 404, Code: "message_not_found", Message: "fixture"}} {
		for _, code := range []int{1, 2, 3} {
			assert := assert.New(t)
			original := &cliError{Kind: "fixture", Code: "fixture", Message: "explicit", Hint: "explicit guidance", ExitCode: code, Err: cause}
			classified := classifyCLIError(nil, original)
			assert.Equal(code, classified.ExitCode)
			assert.Equal(code, ExitCode(original))
			assert.Equal("fixture", classified.Code)
			assert.Equal("explicit guidance", classified.Hint)
		}
	}
}
func TestClassificationSpecialAndDomainStatuses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{ErrSyncPaused, 75},
		{&daemonclient.APIError{Status: 503, Code: "vector_not_enabled", Message: "fixture"}, 5},
		{&daemonclient.APIError{Status: 503, Code: "index_stale", Message: "fixture"}, 6},
		{&daemonclient.APIError{Status: 503, Code: "index_building", Message: "fixture"}, 6},
		{errors.New("unknown command backend failure"), 1},
		{errors.New(`required flag(s) backend failure`), 1},
	} {
		classified := classifyCLIError(nil, tc.err)
		assert.Equal(t, tc.want, classified.ExitCode)
		var out bytes.Buffer
		renderCLIError(&out, classified, true)
		var got struct {
			Error cliErrorRecord `json:"error"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &got))
		assert.Equal(t, tc.want, got.Error.ExitCode)
	}
}
