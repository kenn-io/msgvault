package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Recipient replacement takes the recipient lock before message and person
// rows. IMAP restores and contact changes must wait for that lock before they
// hold any of those rows, or the two can deadlock on PostgreSQL.
func TestRecipientLockPrecedesMailAndContactRowWrites(t *testing.T) {
	for _, operation := range []string{"imap-restore", "contact-point"} {
		t.Run(operation, func(t *testing.T) {
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			if !st.IsPostgreSQL() {
				t.Skip("the recipient lock is a PostgreSQL advisory lock")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			source, err := st.GetOrCreateSource("imap", "lock-order@example.test")
			requirements.NoError(err)
			room, err := st.EnsureConversation(ctx, source.ID, "lock-order-thread", "Lock order")
			requirements.NoError(err)
			participant, err := st.EnsureParticipant("peer@example.test", "Peer", "example.test")
			requirements.NoError(err)
			messageID, err := st.UpsertMessage(&store.Message{
				ConversationID: room, SourceID: source.ID, SourceMessageID: "lock-order",
				RFC822MessageID: sql.NullString{String: "<lock-order@example.test>", Valid: true},
				MessageType:     "email", SenderID: sql.NullInt64{Int64: participant, Valid: true},
			})
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
			requirements.NoError(err)

			var run func() error
			switch operation {
			case "imap-restore":
				requirements.NoError(st.MarkMessageDeleted(source.ID, "lock-order"))
				run = func() error {
					return st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{
						Mailbox: "Recovered",
						State:   store.IMAPFolderState{Mailbox: "Recovered", UIDValidity: 3, UIDNext: 45, HighestModSeq: 30},
						Memberships: []store.IMAPMembershipObservation{{
							Mailbox: "Recovered", UIDValidity: 3, UID: 44, RFC822MessageID: "<lock-order@example.test>",
						}},
					}})
				}
			case "contact-point":
				run = func() error {
					_, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
						AddressKind: store.ContactAddressEmail, OriginalValue: "peer-alias@example.test",
						Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
					})
					return err
				}
			}

			st.DB().SetMaxOpenConns(1)
			var operationPID int
			requirements.NoError(st.DB().QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&operationPID))
			observer, err := sql.Open("pgx", store.DBPathForTest(st))
			requirements.NoError(err)
			defer func() { requirements.NoError(observer.Close()) }()
			holder, err := observer.BeginTx(ctx, nil)
			requirements.NoError(err)
			defer func() { _ = holder.Rollback() }()
			_, err = holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('msgvault.recipient_context_invalidation', 0))`)
			requirements.NoError(err)

			done := make(chan error, 1)
			go func() { done <- run() }()
			requirements.Eventually(func() bool {
				var waiting bool
				err := observer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
					WHERE pid=$1 AND locktype='advisory' AND NOT granted)`, operationPID).Scan(&waiting)
				return err == nil && waiting
			}, 30*time.Second, 10*time.Millisecond, "the operation waits on the recipient lock")
			probe, err := observer.BeginTx(ctx, nil)
			requirements.NoError(err)
			for _, row := range []struct{ query string }{
				{`SELECT id FROM messages WHERE id=$1 FOR UPDATE NOWAIT`},
				{`SELECT id FROM persons WHERE id=$1 FOR UPDATE NOWAIT`},
			} {
				id := messageID
				if row.query == `SELECT id FROM persons WHERE id=$1 FOR UPDATE NOWAIT` {
					id = person.ID
				}
				var locked int64
				requirements.NoError(probe.QueryRowContext(ctx, row.query, id).Scan(&locked),
					"the waiting operation must not hold the row yet")
			}
			requirements.NoError(probe.Rollback())
			requirements.NoError(holder.Commit())
			requirements.NoError(<-done)
		})
	}
}
