package sync

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFullSyncRecordsGenuineThreadMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, thread string }{
		{"root equals message", "thread-message"}, {"provider thread", "provider-thread"}, {"generated fallback", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			env := newTestEnv(t)
			seedMessages(env, 1, 12345, "thread-message")
			env.Mock.UseRawThreadID = true
			env.Mock.Messages["thread-message"].ThreadID = tc.thread
			runFullSync(t, env)
			source, err := env.Store.GetSourceByIdentifier(testEmail)
			require.NoError(err)
			ids, err := env.Store.MessageExistsBatch(source.ID, []string{"thread-message"})
			require.NoError(err)
			raw, err := env.Store.GetAllRawMIMECandidates(source.ID)
			require.NoError(err)
			require.Len(raw, 1)
			require.Equal(ids["thread-message"], raw[0].ID)
			metadata, err := env.Store.GetMessageMetadata(raw[0].ID)
			require.NoError(err)
			var fields map[string]any
			if metadata.Valid {
				require.NoError(json.Unmarshal([]byte(metadata.String), &fields))
			}
			if tc.thread == "" {
				assert.NotContains(fields, "gmail_thread_id")
			} else {
				assert.Equal(tc.thread, fields["gmail_thread_id"])
			}
		})
	}
}
