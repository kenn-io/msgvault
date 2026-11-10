package agentdemotest

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

func TestArchiveCopiesArePrivate(t *testing.T) {
	a, ma := Archive(t)
	b, mb := Archive(t)
	assert.NotEqual(t, a, b)
	assert.Equal(t, ma, mb)
	sa, err := store.OpenForTest(t.Context(), filepath.Join(a, "msgvault.db"))
	require.NoError(t, err)
	defer sa.Close()
	sb, err := store.OpenForTest(t.Context(), filepath.Join(b, "msgvault.db"))
	require.NoError(t, err)
	defer sb.Close()
	_, err = sa.DB().ExecContext(t.Context(), "UPDATE messages SET subject='private copy' WHERE id=?", ma.Messages["canary"])
	require.NoError(t, err)
	var subject string
	require.NoError(t, sb.DB().QueryRowContext(t.Context(), "SELECT subject FROM messages WHERE id=?", mb.Messages["canary"]).Scan(&subject))
	assert.NotEqual(t, "private copy", subject)
	ma.Messages["canary"] = 0
	_, mc := Archive(t)
	assert.NotZero(t, mc.Messages["canary"])
}
func TestStoreOpensAtSchemaVersion(t *testing.T) {
	st, m := Store(t)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(t, len(m.Messages), n)
	assert.Equal(t, m.AsOf, Now(m))
}

func TestArchiveBuildsOnce(t *testing.T) {
	Archive(t)
	Archive(t)
	Archive(t)
	assert.Equal(t, 1, template.builds)
}
