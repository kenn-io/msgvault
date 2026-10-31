package store_test

import (
	"database/sql"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"strconv"
	"testing"
	"time"
)

type timelineActivityFixture struct {
	*storetest.Fixture

	owner, peer int64
}

func newTimelineActivityFixture(t *testing.T) *timelineActivityFixture {
	t.Helper()
	f := storetest.New(t)
	o := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	p := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	return &timelineActivityFixture{Fixture: f, owner: o, peer: p}
}
func (f *timelineActivityFixture) add(t *testing.T, conv, source int64, at time.Time, out bool, copyID string) int64 {
	t.Helper()
	sender, recipient := f.peer, f.owner
	if out {
		sender, recipient = f.owner, f.peer
	}
	m := f.NewMessage().WithSentAt(at).WithIsFromMe(out).Build()
	m.SourceID = source
	m.ConversationID = conv
	m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	if copyID != "" {
		m.RFC822MessageID = sql.NullString{String: copyID, Valid: true}
	}
	id, err := f.Store.UpsertMessage(m)
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, []string{"Recipient"}))
	return id
}
func projectTimelineActivity(t *testing.T, f *timelineActivityFixture, zone string) {
	t.Helper()
	p, err := activity.NewProjector(f.Store, activity.Options{Timezone: zone, BatchSize: 10000})
	require.NoError(t, err)
	_, err = p.RunOnce(t.Context())
	require.NoError(t, err)
}
func timelineBurstQuery() store.BurstQuery {
	return store.BurstQuery{FromDate: "2026-10-01", ToDate: "2026-11-30", Limit: 5000}
}
func TestBurstsReturnUnsavedCorrespondents(t *testing.T) {
	f := newTimelineActivityFixture(t)
	id := f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false, "")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(t, err)
	require.Len(t, got.Bursts, 1)
	b := got.Bursts[0]
	assert.Equal(t, 1, b.Inbound)
	assert.Equal(t, id, b.AnchorMessageID)
	var peer store.ActivityMessageCounterpart
	for _, c := range b.Counterparts {
		if c.ParticipantID == f.peer {
			peer = c
		}
	}
	assert.Zero(t, peer.PersonID)
	assert.Equal(t, "alice@example.com", peer.Address)
	assert.True(t, peer.Counted)
}
func TestBurstsApplySourceScope(t *testing.T) {
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.add(t, f.ConvID, f.Source.ID, at, false, "")
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(t, err)
	conv, err := f.Store.EnsureConversation(other.ID, "other", "Other")
	require.NoError(t, err)
	f.add(t, conv, other.ID, at, false, "")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.ListActivityBurstsContext(sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID}), timelineBurstQuery())
	require.NoError(t, err)
	require.Len(t, got.Bursts, 1)
	assert.Equal(t, f.Source.ID, got.Bursts[0].SourceID)
	assert.Zero(t, got.NoiseOmitted)
}
func TestBurstsCollapseEmailCopiesBeforeGrouping(t *testing.T) {
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	rep := f.add(t, f.ConvID, f.Source.ID, at, false, "<copy@example.com>")
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversation(other.ID, "copy", "Copy")
	require.NoError(t, err)
	f.add(t, conv, other.ID, at, false, "<copy@example.com>")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(t, err)
	require.Len(t, got.Bursts, 1)
	assert.Equal(t, 1, got.Bursts[0].Inbound)
	assert.Equal(t, 2, got.Bursts[0].CopyCounts[rep])
	scoped, err := f.Store.ListActivityBurstsContext(sourcescope.WithSourceIDs(t.Context(), []int64{other.ID}), timelineBurstQuery())
	require.NoError(t, err)
	require.Len(t, scoped.Bursts, 1)
	assert.Equal(t, other.ID, scoped.Bursts[0].SourceID)
}
func TestBurstsAggregateLargeConversationDayExactly(t *testing.T) {
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	// Seed real persisted spine rows in a transaction: pagination must aggregate
	// the entire group, independently of projector batch scheduling.
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for i := range 6000 {
		var id int64
		direction := "inbound"
		sender := f.peer
		if i%2 == 1 {
			direction = "outbound"
			sender = f.owner
		}
		err = tx.QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,sender_id,is_sent,source_is_from_me) VALUES(?,?,?,'email',?,?,TRUE,?) RETURNING id`), f.Source.ID, f.ConvID, fmt.Sprintf("large-%d", i), at.Add(time.Duration(i)*time.Second), sender, i%2 == 1).Scan(&id)
		require.NoError(t, err)
		recipient := f.owner
		if i%2 == 1 {
			recipient = f.peer
		}
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')`), id, recipient)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO activity_events(message_id,ref_kind,source_id,conversation_id,channel,occurred_at,date_origin,date_precision,timezone,utc_offset_minutes,local_date,direction,projected_last_modified,projected_identity_revision,projected_account_identity_revision) VALUES(?,'message',?,?,'email',?,'sent_at','timestamp','UTC',0,'2026-10-22',?,?,0,0)`), id, f.Source.ID, f.ConvID, at.Add(time.Duration(i)*time.Second), direction, at)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	for i := range 3 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("interleave-%d", i), "Other")
		require.NoError(t, err)
		f.add(t, conv, f.Source.ID, at.Add(time.Duration(i)*time.Minute), false, "")
	}
	// Project the three interleaved rows without replacing the large fixture.
	projectTimelineActivity(t, f, "UTC")
	q := timelineBurstQuery()
	q.Limit = 2
	seen := map[string]bool{}
	total := 0
	for {
		page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
		require.NoError(t, err)
		for _, b := range page.Bursts {
			assert.False(t, seen[b.Key])
			seen[b.Key] = true
			total++
			if b.ConversationID == f.ConvID {
				assert.Equal(t, 3000, b.Inbound)
				assert.Equal(t, 3000, b.Outbound)
				assert.Len(t, b.LatestMessageIDs, 5)
			}
		}
		if page.NextKey == nil {
			break
		}
		q.AfterKey = page.NextKey
	}
	assert.Equal(t, 4, total)
}
func TestBurstsDropNoiseAndCount(t *testing.T) {
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := f.add(t, f.ConvID, f.Source.ID, at, false, "")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), id)
	require.NoError(t, err)
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(t, err)
	assert.Empty(t, got.Bursts)
	assert.Equal(t, 1, got.NoiseOmitted)
}
func TestBurstsIncludeNoiseKeepsEligibility(t *testing.T) {
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := f.add(t, f.ConvID, f.Source.ID, at, false, "")
	projectTimelineActivity(t, f, "UTC")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), id)
	require.NoError(t, err)
	q := timelineBurstQuery()
	q.Filters.IncludeNoise = true
	got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	assert.Empty(t, got.Bursts)
}
func TestBurstsExcludedAddressMarksCounterpart(t *testing.T) {
	f := newTimelineActivityFixture(t)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address='noreply@example.com' WHERE id=?`), f.peer)
	require.NoError(t, err)
	f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false, "")
	projectTimelineActivity(t, f, "UTC")
	q := timelineBurstQuery()
	q.Filters.IncludeNoise = true
	got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, got.Bursts, 1)
	for _, c := range got.Bursts[0].Counterparts {
		if c.ParticipantID == f.peer {
			assert.Equal(t, "automated_address", c.Excluded)
			assert.False(t, c.Counted)
		}
	}
}
func TestBurstsCustomExclusionAndDirectLimit(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(strconv.FormatBool(saved), func(t *testing.T) {
			f := newTimelineActivityFixture(t)
			if saved {
				_, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				require.NoError(t, err)
			}
			at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			id := f.add(t, f.ConvID, f.Source.ID, at, true, "")
			bob := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer, bob}, []string{"Alice", "Bob"}))
			projectTimelineActivity(t, f, "UTC")
			q := timelineBurstQuery()
			q.Filters.MaxDirect = 1
			got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(t, err)
			assert.Empty(t, got.Bursts)
			q.Filters.MaxDirect = 25
			q.Filters.Exclusions = persontiers.DefaultConfig()
			q.Filters.Exclusions.ExcludeDomainPatterns = []string{"example.com"}
			got, err = f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(t, err)
			assert.Empty(t, got.Bursts)
		})
	}
}
func TestBurstsKeepLocalDateAcrossDSTDay(t *testing.T) {
	f := newTimelineActivityFixture(t)
	f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), false, "")
	f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), true, "")
	projectTimelineActivity(t, f, "America/New_York")
	got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(t, err)
	require.Len(t, got.Bursts, 1)
	assert.Equal(t, "2026-11-01", got.Bursts[0].LocalDate)
	assert.Equal(t, 1, got.Bursts[0].Inbound)
	assert.Equal(t, 1, got.Bursts[0].Outbound)
}

func TestBurstsOutboundOwnerIsNotNoise(t *testing.T) {
	f := newTimelineActivityFixture(t)
	f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), true, "")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(t, err)
	require.Len(t, got.Bursts, 1)
	assert.Equal(t, 1, got.Bursts[0].Outbound)
	assert.Zero(t, got.NoiseOmitted)
}

func TestBurstsPhoneCounterpartsAreNotEmailExclusions(t *testing.T) {
	f := newTimelineActivityFixture(t)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address=NULL,phone_number='+14155550123' WHERE id=?`), f.peer)
	require.NoError(t, err)
	require.NoError(t, f.Store.SetParticipantIdentifier(f.peer, "phone", "+14155550123"))
	f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), true, "")
	projectTimelineActivity(t, f, "UTC")
	q := timelineBurstQuery()
	q.Filters.IncludeNoise = true
	got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, got.Bursts, 1)
	for _, cp := range got.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			assert.Empty(t, cp.Excluded)
			assert.True(t, cp.Counted)
		}
	}
}
