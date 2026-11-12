package cmd

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXArchiveRegisteredUsernameSurvivesSourceLabelChanges(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	stageXRecord(t, f.cfg, xTestRecord("old-access"))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	source, err := f.st.GetSourceByTypeAndIdentifier(t.Context(), "x", "1001")
	_testRequire.NoError(err)
	_testRequire.NoError(f.st.UpdateSourceDisplayName(source.ID, "Synthetic archive label"))
	state, err := f.st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	encoded, err := json.Marshal(state)
	_testRequire.NoError(err)
	var cached struct {
		Username string `json:"username"`
	}
	_testRequire.NoError(json.Unmarshal(encoded, &cached))
	_testAssert.Equal("example_user", cached.Username, "archive queries require verified cached identity, independently of the editable source label")
	_testAssert.Equal(int32(1), f.meCalls.Load())
}
