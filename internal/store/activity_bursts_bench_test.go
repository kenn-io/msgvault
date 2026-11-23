package store_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// BenchmarkActivityBurstsOwnedArchive measures a one-week read within a large
// projected archive. Real owner identities are essential: an all-observed
// fixture can discard every burst before reaching the expensive reader path.
// Persisted spine rows keep projector scheduling outside the measured operation.
func BenchmarkActivityBurstsOwnedArchive(b *testing.B) {
	s, err := store.Open(filepath.Join(b.TempDir(), "activity.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	require.NoError(b, s.InitSchema())
	policy, err := s.ClaimActivityDirectLimitTransitionContext(b.Context(), store.DefaultMaxDirectActivityCounterparts)
	require.NoError(b, err)
	require.NoError(b, s.CompleteActivityDirectLimitTransitionContext(b.Context(), policy))
	source, err := s.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(b, err)
	owner, err := s.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(b, err)
	peer, err := s.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(b, err)
	require.NoError(b, s.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	conv, err := s.EnsureConversation(source.ID, "scale-thread", "Scale thread")
	require.NoError(b, err)
	tx, err := s.DB().BeginTx(b.Context(), nil)
	require.NoError(b, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(b.Context(), `WITH RECURSIVE sequence(n) AS (
 SELECT 0 UNION ALL SELECT n+1 FROM sequence WHERE n<199999
 ) INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,sender_id,is_sent,source_is_from_me)
 SELECT ?,?,CAST(n AS TEXT),'email',datetime('2015-01-01','+'||CAST(n*5 AS TEXT)||' minutes'),
 CASE WHEN n%2=0 THEN ? ELSE ? END,TRUE,n%2<>0 FROM sequence`, source.ID, conv, peer, owner)
	require.NoError(b, err)
	_, err = tx.ExecContext(b.Context(), `INSERT INTO message_recipients(message_id,participant_id,recipient_type)
 SELECT id,CASE WHEN source_is_from_me THEN ? ELSE ? END,'to' FROM messages`, peer, owner)
	require.NoError(b, err)
	_, err = tx.ExecContext(b.Context(), `INSERT INTO activity_events(message_id,ref_kind,source_id,conversation_id,channel,occurred_at,date_origin,date_precision,timezone,utc_offset_minutes,local_date,direction,projected_last_modified,projected_identity_revision,projected_account_identity_revision)
 SELECT id,'message',source_id,conversation_id,'email',sent_at,'sent_at','timestamp','UTC',0,date(sent_at),
 CASE WHEN source_is_from_me THEN 'outbound' ELSE 'inbound' END,sent_at,0,0 FROM messages`)
	require.NoError(b, err)
	require.NoError(b, tx.Commit())

	q := store.BurstQuery{FromDate: "2015-01-15", ToDate: "2015-01-21", Limit: 5000}
	b.ResetTimer()
	for b.Loop() {
		page, err := s.ListActivityBurstsContext(b.Context(), q)
		require.NoError(b, err)
		require.Len(b, page.Bursts, 7)
		for _, burst := range page.Bursts {
			require.Equal(b, 144, burst.Inbound)
			require.Equal(b, 144, burst.Outbound)
			require.Len(b, burst.LatestMessageIDs, 5)
		}
		require.Zero(b, page.NoiseOmitted)
		require.Nil(b, page.NextKey)
	}
	b.ReportMetric(200000, "archive-messages")
	b.ReportMetric(2016, "window-messages")
}
