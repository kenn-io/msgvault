package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pause at the real hydration query so an identity write commits between
// selection and hydration. Both results must describe the selection's state.
func TestActivityReadersKeepIdentitySnapshot(t *testing.T) {
	for _, reader := range []string{"bursts", "messages"} {
		t.Run(reader, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			gate := newGmailAuditSnapshotGate(0)
			gate.query = "counterpart_flags AS"
			var st *Store
			if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
				st = newGmailAuditSnapshotPostgresStore(t, gate)
			} else {
				st = newGmailAuditSnapshotSQLiteStore(t, gate)
			}
			policy, err := st.ClaimActivityDirectLimitTransitionContext(t.Context(), 25)
			require.NoError(err)
			require.NoError(st.CompleteActivityDirectLimitTransitionContext(t.Context(), policy))
			source, err := st.GetOrCreateSource("gmail", "owner@example.com")
			require.NoError(err)
			require.NoError(st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
			conv, err := st.EnsureConversation(source.ID, "snapshot", "Snapshot")
			require.NoError(err)
			owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
			require.NoError(err)
			peer, err := st.EnsureParticipant("noreply@example.com", "Peer", "example.com")
			require.NoError(err)
			at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			id, err := st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "snapshot", MessageType: "email", SenderID: sql.NullInt64{Int64: peer, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}})
			require.NoError(err)
			require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{peer}, []string{"Peer"}))
			require.NoError(st.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO activity_events(message_id,ref_kind,source_id,conversation_id,channel,occurred_at,date_origin,date_precision,timezone,utc_offset_minutes,local_date,direction,projected_last_modified,projected_identity_revision,projected_account_identity_revision) VALUES(?,'message',?,?,'email',?,'sent_at','timestamp','UTC',0,'2026-10-22','inbound',?,0,0)`), id, source.ID, conv, at, at)
			require.NoError(err)
			writer, err := OpenForTest(st.dbPath)
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(writer.Close()) })
			ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate), 30*time.Second)
			defer cancel()
			defer gate.resume()
			type result struct {
				counterparts []ActivityMessageCounterpart
				inbound      int
				err          error
			}
			read := func(ctx context.Context) result {
				filters := ActivityFilters{IncludeNoise: true}
				if reader == "bursts" {
					page, err := st.ListActivityBurstsContext(ctx, BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 10, Filters: filters})
					if err != nil || len(page.Bursts) != 1 {
						return result{err: err}
					}
					return result{counterparts: page.Bursts[0].Counterparts, inbound: page.Bursts[0].Inbound}
				}
				page, err := st.ListActivityMessagesContext(ctx, MessageQuery{ConversationID: conv, Limit: 10, Filters: filters})
				if err != nil || len(page) != 1 {
					return result{err: err}
				}
				return result{counterparts: page[0].Counterparts}
			}
			done := make(chan result, 1)
			go func() { done <- read(ctx) }()
			select {
			case <-gate.paused:
			case <-ctx.Done():
				require.FailNow("hydration did not reach the gate")
			}
			require.NoError(writer.SetParticipantIdentifier(peer, "matrix", "@peer:example.com"))
			gate.resume()
			got := <-done
			require.NoError(got.err)
			assert.Zero(got.inbound)
			var found bool
			for _, cp := range got.counterparts {
				if cp.ParticipantID == peer {
					found = true
					assert.False(cp.Counted)
					assert.Equal("automated_address", cp.Excluded)
				}
			}
			require.True(found)
			fresh := read(t.Context())
			require.NoError(fresh.err)
			require.Len(fresh.counterparts, 2)
			if reader == "bursts" {
				assert.Equal(1, fresh.inbound)
			}
			for _, cp := range fresh.counterparts {
				if cp.ParticipantID == peer {
					assert.True(cp.Counted)
					assert.Empty(cp.Excluded)
				}
			}
		})
	}
}
