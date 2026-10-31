//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package imessage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
)

type generatedFTSFixture struct {
	db       *sql.DB
	client   *Client
	store    *store.Store
	sourceID int64
}

func newGeneratedFTSFixture(t *testing.T) generatedFTSFixture {
	t.Helper()
	require := require.New(t)
	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message (date,text) VALUES (725760000, '')`)
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	require.True(st.FTS5Available())
	src, err := st.GetOrCreateSource("apple_messages", "fixture")
	require.NoError(err)
	return generatedFTSFixture{db: db, client: client, store: st, sourceID: src.ID}
}

// Hegel bundles its native engine only for the platforms selected above.
func TestImportIndexesGeneratedBodies(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite chat.db and archive fixture")
	fixture := newGeneratedFTSFixture(t)
	const tokenPrefix = "generatedbodytoken"
	tokenCounter := uint64(0)
	lastImportedToken := ""
	hegel.Test(t, func(ht *hegel.T) {
		tokenCounter++
		token := fmt.Sprintf("%s%06d", tokenPrefix, tokenCounter)
		text := []rune(hegel.Draw(ht, hegel.Text()))
		text = text[:min(len(text), 512)] // Bound materialized fixture size, not the generated domain.
		body := strings.Repeat("padding ", 20) + " " + token + " " + string(text)
		outgoing := hegel.Draw(ht, hegel.Booleans())
		service := hegel.Draw(ht, hegel.SampledFrom([]string{"SMS", "iMessage"}))
		WithOwnerHandle(hegel.Draw(ht, hegel.SampledFrom([]string{"", "owner@example.test", "+12025550100"})))(fixture.client)
		handle := hegel.Draw(ht, hegel.Optional(hegel.SampledFrom([]string{"peer@example.test", "+12025550101"})))
		_, err := fixture.db.Exec(`UPDATE handle SET id = ?`, handle)
		require.NoError(ht, err)
		_, err = fixture.db.Exec(`UPDATE message SET text = ?, service = ?, is_from_me = ?`, body, service, outgoing)
		require.NoError(ht, err)
		_, err = fixture.client.Import(context.Background(), fixture.store, fixture.sourceID)
		require.NoError(ht, err)

		var id int64
		require.NoError(ht, fixture.store.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ?`, fixture.sourceID).Scan(&id))
		var importedSubject, importedBody, importedFrom, importedTo, importedCC string
		require.NoError(ht, fixture.store.DB().QueryRow(`SELECT subject,body,from_addr,to_addr,cc_addr FROM messages_fts WHERE rowid = ?`, id).Scan(
			&importedSubject, &importedBody, &importedFrom, &importedTo, &importedCC,
		))
		assert.Equal(ht, body, importedBody)
		ids, total, err := fixture.store.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{token}}, 10)
		require.NoError(ht, err)
		assert.Equal(ht, int64(1), total)
		assert.Equal(ht, []int64{id}, ids)
		if lastImportedToken != "" {
			ids, _, err = fixture.store.SearchMessageIDsQueryContext(context.Background(), &search.Query{TextTerms: []string{lastImportedToken}}, 10)
			require.NoError(ht, err)
			assert.Empty(ht, ids)
		}
		lastImportedToken = token

		_, err = fixture.store.DB().Exec(`DELETE FROM messages_fts WHERE rowid = ?`, id)
		require.NoError(ht, err)
		inserted, err := fixture.store.BackfillFTS(nil)
		require.NoError(ht, err)
		assert.Equal(ht, int64(1), inserted)
		var backfillSubject, backfillBody, backfillFrom, backfillTo, backfillCC string
		require.NoError(ht, fixture.store.DB().QueryRow(`SELECT subject,body,from_addr,to_addr,cc_addr FROM messages_fts WHERE rowid = ?`, id).Scan(
			&backfillSubject, &backfillBody, &backfillFrom, &backfillTo, &backfillCC,
		))
		assert.Equal(ht, body, backfillBody)
		assert.Equal(ht, importedSubject, backfillSubject)
		assert.Equal(ht, importedFrom, backfillFrom)
		assert.Equal(ht, importedTo, backfillTo)
		assert.Equal(ht, importedCC, backfillCC)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}
