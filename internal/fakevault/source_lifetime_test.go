package fakevault_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fakevault"
	"go.kenn.io/msgvault/internal/store"
	"path/filepath"
	"testing"
)

func TestAppendRefusesRemovedOrReimportedSourceLifetime(t *testing.T) {
	for _, reimported := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed", true: "reimported"}[reimported], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			opts := fakevault.Options{Dir: t.TempDir(), Messages: 8, Seed: 42}
			_, err := fakevault.Generate(t.Context(), opts)
			require.NoError(err)
			st, err := store.Open(filepath.Join(opts.Dir, "msgvault.db"))
			require.NoError(err)
			defer func() { require.NoError(st.Close()) }()
			source, err := st.GetSourceByID(1)
			require.NoError(err)
			require.NoError(st.RemoveSource(source.ID))
			var recreated *store.Source
			if reimported {
				recreated, err = st.GetOrCreateSource(source.SourceType, source.Identifier)
				require.NoError(err)
				assert.Greater(recreated.ID, source.ID, "ordinary reimport allocates a new source lifetime")
			}
			var messages, participants int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&participants))
			opts.Append = true
			_, err = fakevault.Generate(t.Context(), opts)
			require.Error(err, "append cannot resurrect an issued fixed source ID")
			var afterMessages, afterParticipants int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&afterMessages))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&afterParticipants))
			assert.Equal(messages, afterMessages)
			assert.Equal(participants, afterParticipants)
			_, err = st.GetSourceByID(source.ID)
			require.Error(err, "append never restores the retired fixed source")
			if reimported {
				current, err := st.GetSourceByID(recreated.ID)
				require.NoError(err)
				assert.Equal(source.Identifier, current.Identifier)
			}
		})
	}
}
