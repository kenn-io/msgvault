package cmd

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// TestOwnerAttributionParityStoreVersusCache pins the DuckDB owner mirror in
// build_cache.go to the store's attribution SQL: every message's is_from_me
// and every source's owner participants must agree row for row.
func TestOwnerAttributionParityStoreVersusCache(t *testing.T) {
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	sourceA, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	sourceB, err := st.GetOrCreateSource("whatsapp", "+15550100")
	require.NoError(err)
	identities := map[int64][]string{
		sourceA.ID: {"alice@example.com", "alias@example.com", "carol@example.com"},
		sourceB.ID: {"+15550100", "alice@example.com"},
	}
	for source, addresses := range identities {
		for _, address := range addresses {
			require.NoError(st.AddAccountIdentity(source, address, "manual"))
		}
	}

	caseSender, err := st.EnsureParticipant("Alice@Example.COM", "Alice", "example.com")
	require.NoError(err)
	aliasOnly, err := st.EnsureParticipantByPhone("+15550111", "Alias", "whatsapp")
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifier(aliasOnly, "email", "Alias@Example.com"))
	guarded, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifier(guarded, "email", "carol@example.com"))
	phone, err := st.EnsureParticipantByPhone("+15550100", "Phone", "whatsapp")
	require.NoError(err)

	sentAt := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	messageIDs := map[string]int64{}
	addMessage := func(source int64, id string, sender int64, envelope ...store.RecipientSet) {
		conv, err := st.EnsureConversation(source, "thread-"+id, "Thread")
		require.NoError(err)
		messageID, err := st.PersistMessage(&store.MessagePersistData{
			Message: &store.Message{
				ConversationID: conv, SourceID: source, SourceMessageID: id, MessageType: "email",
				SenderID: sql.NullInt64{Int64: sender, Valid: true},
				SentAt:   sql.NullTime{Time: sentAt, Valid: true},
			},
			Recipients: envelope,
		})
		require.NoError(err)
		messageIDs[id] = messageID
	}
	fromEnvelope := func(participant int64, address string) store.RecipientSet {
		return store.RecipientSet{Type: "from", ParticipantIDs: []int64{participant},
			DisplayNames: []string{""}, EmailAddresses: []string{address}}
	}
	for _, sender := range []int64{caseSender, aliasOnly, guarded, phone} {
		addMessage(sourceA.ID, fmt.Sprintf("a-fallback-%d", sender), sender)
		addMessage(sourceB.ID, fmt.Sprintf("b-fallback-%d", sender), sender)
	}
	// Each envelope disagrees with its sender, so only the envelope branch explains the result.
	addMessage(sourceA.ID, "a-envelope-disagrees", caseSender, fromEnvelope(guarded, "bob@example.com"))
	addMessage(sourceA.ID, "a-envelope-owner", guarded, fromEnvelope(caseSender, "Alice@Example.COM"))

	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err)
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckdb.Close() })

	type attribution struct {
		ID       int64
		IsFromMe bool
	}
	readAttribution := func(db *sql.DB, query string, args ...any) []attribution {
		rows, err := db.Query(query, args...)
		require.NoError(err)
		defer func() { _ = rows.Close() }()
		var got []attribution
		for rows.Next() {
			var row attribution
			require.NoError(rows.Scan(&row.ID, &row.IsFromMe))
			got = append(got, row)
		}
		require.NoError(rows.Err())
		return got
	}
	storeRows := readAttribution(st.DB(), `SELECT id, is_from_me FROM messages ORDER BY id`)
	require.Len(storeRows, 10)
	fromMe := 0
	for _, row := range storeRows {
		if row.IsFromMe {
			fromMe++
		}
	}
	envelopeFromMe := map[int64]bool{}
	for _, row := range storeRows {
		envelopeFromMe[row.ID] = row.IsFromMe
	}
	require.False(envelopeFromMe[messageIDs["a-envelope-disagrees"]], "a non-owner envelope overrides an owner sender")
	require.True(envelopeFromMe[messageIDs["a-envelope-owner"]], "an owner envelope overrides a non-owner sender")
	require.NotZero(fromMe, "the fixture must attribute some messages to the owner")
	require.Less(fromMe, len(storeRows), "the fixture must leave some messages unattributed")
	require.Equal(storeRows, readAttribution(duckdb,
		`SELECT id, is_from_me FROM read_parquet(?, hive_partitioning=true) ORDER BY id`,
		filepath.Join(analyticsDir, "messages", "**", "*.parquet")))

	type owner struct{ Source, Participant int64 }
	var storeOwners []owner
	for source, addresses := range identities {
		for _, address := range addresses {
			resolved, err := st.ResolveAccountIdentityContext(t.Context(), source, address)
			require.NoError(err)
			for _, participant := range resolved.ParticipantIDs {
				if !slices.Contains(storeOwners, owner{source, participant}) {
					storeOwners = append(storeOwners, owner{source, participant})
				}
			}
		}
	}
	slices.SortFunc(storeOwners, func(a, b owner) int {
		if a.Source != b.Source {
			return int(a.Source - b.Source)
		}
		return int(a.Participant - b.Participant)
	})
	rows, err := duckdb.Query(`SELECT source_id, participant_id FROM read_parquet(?)
		GROUP BY source_id, participant_id ORDER BY source_id, participant_id`,
		filepath.Join(analyticsDir, "owner_participants", "*.parquet"))
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var cacheOwners []owner
	for rows.Next() {
		var row owner
		require.NoError(rows.Scan(&row.Source, &row.Participant))
		cacheOwners = append(cacheOwners, row)
	}
	require.NoError(rows.Err())
	require.NotEmpty(storeOwners)
	require.Equal(storeOwners, cacheOwners)
}
