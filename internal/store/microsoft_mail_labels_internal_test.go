package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMicrosoftMailFoldersStopAtFailedNameLookup(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite statement cancellation fixture")
	}
	assert := assert.New(t)
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "folders.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("msmail", "owner@example.test")
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	trigger := cancelAtStatement{stop: "SELECT id,source_label_id FROM labels", cancel: cancel}
	trigger.install(st.db)

	_, err = st.EnsureMicrosoftMailFoldersContext(ctx, source.ID, map[string]LabelInfo{
		"inbox": {Name: "Inbox", Type: "system"},
	})

	require.True(trigger.fired)
	require.ErrorIs(err, context.Canceled)
	assert.ErrorContains(err, "check Microsoft folder name")
}
