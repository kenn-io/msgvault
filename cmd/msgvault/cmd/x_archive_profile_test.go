package cmd

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXArchiveRegisteredUsernameSurvivesSourceLabelChanges(t *testing.T) {
	f := newXRegistrationFixture(t)
	stageXRecord(t, f.cfg, xTestRecord("old-access"))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	require.NoError(t, err)
	source, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	require.NoError(t, err)
	require.NoError(t, f.st.UpdateSourceDisplayName(source.ID, "Synthetic archive label"))
	state, err := f.st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	var cached struct {
		Username string `json:"username"`
	}
	require.NoError(t, json.Unmarshal(encoded, &cached))
	assert.Equal(t, "example_user", cached.Username, "archive queries require verified cached identity, independently of the editable source label")
	assert.Equal(t, int32(1), f.meCalls.Load())
}
