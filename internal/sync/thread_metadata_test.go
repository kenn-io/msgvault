package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFullSyncScoresStoredThreadEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, thread string
		wantQuality  int
	}{
		{"root equals message", "thread-message", 2},
		{"provider thread", "provider-thread", 3},
		{"generated fallback", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			env := newTestEnv(t)
			seedMessages(env, 1, 12345, "thread-message")
			env.Mock.Messages["thread-message"].Raw = []byte("Message-ID: <thread@example.test>\r\nSubject: Thread\r\n\r\nBody")
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
			assert.Equal(tc.wantQuality, raw[0].MetadataQuality)
		})
	}
}
