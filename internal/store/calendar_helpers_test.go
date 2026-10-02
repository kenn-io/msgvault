package store_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestSetMessageMetadata_RoundTrip proves the new metadata write path persists
// JSON to the messages.metadata column (JSON on SQLite, JSONB on PG) and reads
// back semantically intact. Runs under both dialects (make test-pg) so the
// JSONBindExpr cast is exercised on Postgres.
func TestSetMessageMetadata_RoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	msgID := f.CreateMessage("cal-meta-1")

	meta := `{"status":"confirmed","all_day":false,"end":"2024-05-01T11:00:00Z","recurrence":["RRULE:FREQ=WEEKLY"]}`
	require.NoError(f.Store.SetMessageMetadata(msgID, sql.NullString{String: meta, Valid: true}))

	var got sql.NullString
	err := f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT metadata FROM messages WHERE id = ?"), msgID,
	).Scan(&got)
	require.NoError(err)
	require.True(got.Valid, "metadata should be non-NULL after write")

	// Compare semantically: PG JSONB normalizes whitespace/key order, so a raw
	// string compare would be brittle across dialects.
	var parsed map[string]any
	require.NoError(json.Unmarshal([]byte(got.String), &parsed))
	assert.Equal("confirmed", parsed["status"])
	assert.Equal(false, parsed["all_day"])
	assert.Equal("2024-05-01T11:00:00Z", parsed["end"])
	rec, ok := parsed["recurrence"].([]any)
	require.True(ok, "recurrence should round-trip as a JSON array")
	require.Len(rec, 1)
	assert.Equal("RRULE:FREQ=WEEKLY", rec[0])
}

// TestSetMessageMetadata_Clear proves an invalid sql.NullString writes SQL NULL,
// clearing a previously-set metadata value.
func TestSetMessageMetadata_Clear(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	msgID := f.CreateMessage("cal-meta-clear")
	require.NoError(f.Store.SetMessageMetadata(msgID, sql.NullString{String: `{"a":1}`, Valid: true}))

	require.NoError(f.Store.SetMessageMetadata(msgID, sql.NullString{}))

	var got sql.NullString
	err := f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT metadata FROM messages WHERE id = ?"), msgID,
	).Scan(&got)
	require.NoError(err)
	assert.False(got.Valid, "metadata should be NULL after clear")
}

// TestGetSourcesByTypeAndAccount proves the account-scoped source lookup that
// calendar sync uses to enumerate one OAuth account's calendars: it filters by
// source_type AND sync_config.account_email, decoupled from the per-source
// identifier (the natural calendar key). A source with NULL/garbage sync_config
// is skipped, not fatal.
func TestGetSourcesByTypeAndAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	mk := func(identifier, accountEmail string) *store.Source {
		src, err := st.GetOrCreateSource("gcal", identifier)
		require.NoError(err)
		cfg, err := json.Marshal(map[string]string{
			"account_email": accountEmail,
			"calendar_id":   identifier,
		})
		require.NoError(err)
		require.NoError(st.UpdateSourceSyncConfig(src.ID, string(cfg)))
		return src
	}

	a1 := mk("a@example.com/primary", "a@example.com")
	a2 := mk("a@example.com/work", "a@example.com")
	bSrc := mk("b@example.com/primary", "b@example.com")

	// A gcal source with NULL sync_config must be skipped (not matched, not fatal).
	noCfg, err := st.GetOrCreateSource("gcal", "orphan-calendar")
	require.NoError(err)
	// A same-email gmail source must not bleed into the gcal-typed result.
	_, err = st.GetOrCreateSource("gmail", "a@example.com")
	require.NoError(err)

	got, err := st.GetSourcesByTypeAndAccount("gcal", "a@example.com")
	require.NoError(err)
	require.Len(got, 2)
	ids := map[int64]bool{}
	for _, s := range got {
		ids[s.ID] = true
		assert.Equal("gcal", s.SourceType)
	}
	assert.True(ids[a1.ID])
	assert.True(ids[a2.ID])
	assert.False(ids[bSrc.ID], "account B's calendar must not be returned")
	assert.False(ids[noCfg.ID], "NULL sync_config source must be skipped")

	none, err := st.GetSourcesByTypeAndAccount("gcal", "nobody@example.com")
	require.NoError(err)
	assert.Empty(none)
}

func TestGetSourcesByTypeAndAccount_EmailCaseInsensitive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("gcal", "mixed@example.com/primary")
	require.NoError(err)
	cfg, err := json.Marshal(map[string]string{
		"account_email": "Mixed.Case@Example.COM",
		"calendar_id":   "primary",
	})
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(src.ID, string(cfg)))

	got, err := st.GetSourcesByTypeAndAccount("gcal", "mixed.case@example.com")
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal(src.ID, got[0].ID)
}
