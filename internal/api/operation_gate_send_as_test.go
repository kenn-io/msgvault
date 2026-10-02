package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendAsConfirmationTakesMutationGate(t *testing.T) {
	for _, tc := range []struct {
		args []string
		skip bool
	}{
		{[]string{"draft-send-as", "owner@example.test", "--json"}, true},
		{[]string{"draft-send-as", "owner@example.test", "--confirm", "alias@example.test"}, false},
		{[]string{"draft-send-as", "owner@example.test", "--confirm=alias@example.test"}, false},
	} {
		t.Run(tc.args[len(tc.args)-1], func(t *testing.T) {
			body, err := json.Marshal(CLIRunRequest{Args: tc.args})
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			_, skip, err := cliRunGateDecision(r, requestAuthentication{Mode: AuthModeAPIKey})
			require.NoError(t, err)
			assert.Equal(t, tc.skip, skip)
		})
	}
}
