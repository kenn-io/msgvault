package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXRestoreIdentitySurvivesReconnectButNotSourceRecreation(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	stageXRecord(t, f.cfg, xTestRecord("old-access"))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	source, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	_testRequire.NoError(err)
	first, err := f.st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(first.InstanceID, 64)
	stageXRecord(t, f.cfg, xTestRecord("new-access"))
	_, err = f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	reconnected, err := f.st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(first.InstanceID, reconnected.InstanceID)
	_testAssert.Greater(reconnected.Generation, first.Generation)
	_testRequire.NoError(f.st.RemoveSource(source.ID))
	stageXRecord(t, f.cfg, xTestRecord("latest-access"))
	_, err = f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	recreated, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	_testRequire.NoError(err)
	current, err := f.st.LoadXControlContext(t.Context(), recreated.ID)
	_testRequire.NoError(err)
	_testRequire.Len(current.InstanceID, 64)
	_testAssert.NotEqual(first.InstanceID, current.InstanceID)
	_testAssert.Equal(int32(3), f.meCalls.Load(), "incarnation binding adds no provider reads to verified registration")
}
