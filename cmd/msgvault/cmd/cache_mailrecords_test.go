package cmd

import (
	"database/sql"
	"encoding/json/jsontext"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mailrecords"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMessageRecordSQLValueNormalizesDecimalJSONValue(t *testing.T) {
	got := messageRecordSQLValue(jsontext.Value("19.95"))
	assert.IsType(t, "", got)
	assert.Equal(t, "19.95", got)
}

func TestMailRecordsCachePublicationAndBodyRepair(t *testing.T) {
	for _, mode := range []string{"platform", "csv"} {
		t.Run(mode, func(t *testing.T) {
			force := ""
			if mode == "csv" {
				force = "1"
			}
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", force)
			testMailRecordsCachePublicationAndBodyRepair(t)
		})
	}
}

func testMailRecordsCachePublicationAndBodyRepair(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	dbPath, analytics := filepath.Join(root, "archive.db"), filepath.Join(root, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "orders", "Orders")
	require.NoError(t, err)
	insert := func(key, subject, text, html string) int64 {
		t.Helper()
		id, insertErr := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: key,
			MessageType: "email", Subject: sql.NullString{String: subject, Valid: true},
			SentAt: sql.NullTime{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Valid: true}})
		require.NoError(t, insertErr)
		require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: text, Valid: true}, sql.NullString{String: html, Valid: true}))
		return id
	}
	body := `<p>Order B-42, 19.95 USD</p><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-42","price":"19.95","priceCurrency":"USD"}</script>`
	first := insert("first", "Order", "Order B-42, 19.95 USD", body)
	insert("credential", "Your sign-in verification code", "Your sign-in verification code is 654321", body)
	oversizedJSONLD := `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"TOO-LARGE"}</script>`
	oversizedHTML := oversizedJSONLD + strings.Repeat(" ", mailrecords.MaxHTMLBytes-len(oversizedJSONLD)+1)
	insert("oversized", "Large order", "", oversizedHTML)
	_, err = buildCache(dbPath, analytics, true)
	require.NoError(t, err)
	read := func(statement string) *query.QueryResult {
		t.Helper()
		engine, openErr := query.NewArchiveDuckDBEngine(t.Context(), analytics)
		require.NoError(t, openErr)
		defer func() { require.NoError(t, engine.Close()) }()
		result, queryErr := engine.QuerySQL(t.Context(), statement)
		require.NoError(t, queryErr)
		return result
	}
	result := read("SELECT message_id, kind, order_number, CAST(amount AS VARCHAR), currency, basis FROM message_records")
	require.Len(t, result.Rows, 1, "credential messages must not enter any record column")
	assert.Equal(t, []any{first, "order", "B-42", "19.9500", "USD", "jsonld"}, result.Rows[0])
	require.NotNil(t, result.Cache)
	assert.NotEmpty(t, result.Cache.Generation)
	var canonical int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='message_records'").Scan(&canonical))
	assert.Zero(t, canonical)
	assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
	require.NoError(t, st.UpsertMessageBody(first, sql.NullString{String: "Order B-42, 19.95 USD", Valid: true}, sql.NullString{String: body, Valid: true}))
	assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild, "an identical resync must not invalidate the projection")

	// A new body above the publication boundary remains an append; the
	// derived dataset reuses the committed prefix and extracts only new IDs.
	second := insert("second", "Order", "Order B-42, 19.95 USD", body)
	state := cacheNeedsBuild(dbPath, analytics)
	assert.True(t, state.HasNew)
	assert.False(t, state.FullRebuild)
	var stagedIDs []int64
	savedCandidateHook := stageMailRecordsCandidateHook
	t.Cleanup(func() { stageMailRecordsCandidateHook = savedCandidateHook })
	stageMailRecordsCandidateHook = func(id int64) { stagedIDs = append(stagedIDs, id) }
	_, err = buildCacheAuto(dbPath, analytics)
	require.NoError(t, err)
	assert.Equal(t, []int64{second}, stagedIDs, "incremental builds must hydrate only IDs above the committed boundary")
	stageMailRecordsCandidateHook = nil
	messageIDs := read("SELECT message_id FROM message_records ORDER BY message_id").Rows
	require.Len(t, messageIDs, 2)
	assert.Equal(t, first, messageIDs[0][0])
	assert.Equal(t, second, messageIDs[1][0])

	// Changed HTML with identical text and archive garbage collection both
	// invalidate the existing records, without timestamp sleeps.
	require.NoError(t, st.UpsertMessageBody(first, sql.NullString{String: "Order B-42, 19.95 USD", Valid: true}, sql.NullString{String: "<p>No structured record</p>", Valid: true}))
	assert.True(t, cacheNeedsBuild(dbPath, analytics).FullRebuild)
	_, err = buildCacheAuto(dbPath, analytics)
	require.NoError(t, err)
	assert.EqualValues(t, 1, read("SELECT count(*) FROM message_records").Rows[0][0])
	_, err = st.DB().Exec("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP")
	require.NoError(t, err)
	plan, err := st.PlanGCContext(t.Context())
	require.NoError(t, err)
	_, err = st.ExecuteGCContext(t.Context(), plan)
	require.NoError(t, err)
	assert.True(t, cacheNeedsBuild(dbPath, analytics).FullRebuild)
	_, err = buildCacheAuto(dbPath, analytics)
	require.NoError(t, err)
	assert.EqualValues(t, 0, read("SELECT count(*) FROM message_records").Rows[0][0])
	readiness, err := query.InspectCacheReadiness(analytics)
	require.NoError(t, err)
	assert.Equal(t, query.CacheReady, readiness, "zero records still need a schema-only replacement file")
	assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
}

func TestMailRecordsCachePinsBodySnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
	root := t.TempDir()
	dbPath, analytics := filepath.Join(root, "archive.db"), filepath.Join(root, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	_, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "first", "Order", "Order OLD")
	requirements.NoError(err)
	_, err = st.DB().Exec("UPDATE messages SET sent_at = ? WHERE id = ?", time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), id)
	requirements.NoError(err)
	body := func(number string) string {
		return `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"` + number + `"}</script>`
	}
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{}, sql.NullString{String: body("OLD"), Valid: true}))
	saved := buildCacheAfterSnapshotHook
	t.Cleanup(func() { buildCacheAfterSnapshotHook = saved })
	buildCacheAfterSnapshotHook = func() {
		requirements.NoError(st.UpsertMessageBody(id, sql.NullString{}, sql.NullString{String: body("NEW"), Valid: true}))
	}
	_, err = buildCache(dbPath, analytics, true)
	requirements.NoError(err)
	engine, err := query.NewArchiveDuckDBEngine(t.Context(), analytics)
	requirements.NoError(err)
	defer func() { requirements.NoError(engine.Close()) }()
	result, err := engine.QuerySQL(t.Context(), "SELECT order_number FROM message_records")
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal("OLD", result.Rows[0][0])
	assertions.True(cacheNeedsBuild(dbPath, analytics).FullRebuild, "a later body cannot be acknowledged by the older snapshot marker")
}

func TestMailRecordsCacheKeepsReceiptWithGenericSignInNavigation(t *testing.T) {
	root := t.TempDir()
	dbPath, analytics := filepath.Join(root, "archive.db"), filepath.Join(root, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "orders", "Orders")
	require.NoError(t, err)
	bodyHTML := `<p>Your order B-43 is confirmed. Total: 1299.00 CHF. Sign in to view your order.</p><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-43"}</script>`
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "navigation",
		MessageType: "email", Subject: sql.NullString{String: "Your order is confirmed", Valid: true},
		SentAt: sql.NullTime{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: "Your order B-43 is confirmed. Total: 1299.00 CHF. Sign in to view your order.", Valid: true}, sql.NullString{String: bodyHTML, Valid: true}))

	_, err = buildCache(dbPath, analytics, true)
	require.NoError(t, err)
	engine, err := query.NewArchiveDuckDBEngine(t.Context(), analytics)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	result, err := engine.QuerySQL(t.Context(), "SELECT message_id, kind, order_number FROM message_records")
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, []any{id, "order", "B-43"}, result.Rows[0])
}

func TestMailRecordsCacheKeepsInvoicesAndBookingsWithGenericSignIn(t *testing.T) {
	root := t.TempDir()
	dbPath, analytics := filepath.Join(root, "archive.db"), filepath.Join(root, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "receipts", "Receipts")
	require.NoError(t, err)
	insert := func(key, text, jsonld string) int64 {
		t.Helper()
		id, insertErr := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: key,
			MessageType: "email", Subject: sql.NullString{String: "Receipt", Valid: true},
			SentAt: sql.NullTime{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Valid: true}})
		require.NoError(t, insertErr)
		html := `<p>` + text + `</p><script type="application/ld+json">` + jsonld + `</script>`
		require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: text, Valid: true}, sql.NullString{String: html, Valid: true}))
		return id
	}
	invoice := insert("invoice", "Total 1299 CHF. Sign in to view your invoice.", `{"@context":"https://schema.org","@type":"Invoice","confirmationNumber":"INV-47","totalPaymentDue":{"value":1299,"currency":"CHF"}}`)
	booking := insert("booking", "Confirmation number: ABC123. Sign in to manage your booking.", `{"@context":"https://schema.org","@type":"FlightReservation","reservationNumber":"ABC123"}`)
	_, err = buildCache(dbPath, analytics, true)
	require.NoError(t, err)
	engine, err := query.NewArchiveDuckDBEngine(t.Context(), analytics)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	result, err := engine.QuerySQL(t.Context(), "SELECT message_id, kind, currency, confirmation_code FROM message_records ORDER BY message_id")
	require.NoError(t, err)
	require.Len(t, result.Rows, 2)
	assert.Equal(t, []any{invoice, "invoice", "CHF", nil}, result.Rows[0])
	assert.Equal(t, []any{booking, "flight", nil, "ABC123"}, result.Rows[1])
}
