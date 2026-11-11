package cmd

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIOutputInvalidUTF8StillProducesJSON(t *testing.T) {
	var out bytes.Buffer
	renderCLIError(&out, &cliError{Code: "validation", Message: string([]byte{255}), Hint: "check flags", ExitCode: 3}, true)
	var got struct {
		Error cliErrorRecord `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, "\uFFFD", got.Error.Message)
}
func TestCLIOutputKeepsReconciliationIntegers(t *testing.T) {
	details := jsontext.Value(`{"message_id":9223372036854775807,"manual_reconciliation":true}`)
	var out bytes.Buffer
	renderCLIError(&out, &cliError{Code: "conflict", Message: "fixture", Hint: "inspect draft", ExitCode: 6, Details: details}, true)
	var got struct {
		Error struct {
			Details jsontext.Value `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, details, got.Error.Details)
}
