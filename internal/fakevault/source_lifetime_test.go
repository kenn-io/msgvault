package fakevault_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fakevault"
	"go.kenn.io/msgvault/internal/store"
	"path/filepath"
	"testing"
)

func TestAppendRefusesRemovedOrReplacedSourceLifetime(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed", true: "replaced"}[replaced], func(t *testing.T) {
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
			if replaced {
				_, err = st.DB().Exec(`INSERT INTO sources(id,source_type,identifier) VALUES(1,'gmail','replacement@example.test')`)
				require.NoError(err)
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
			if replaced {
				var identifier string
				require.NoError(st.DB().QueryRow(`SELECT identifier FROM sources WHERE id=1`).Scan(&identifier))
				assert.Equal("replacement@example.test", identifier)
			} else {
				_, err = st.GetSourceByID(1)
				assert.Error(err)
			}
		})
	}
}
