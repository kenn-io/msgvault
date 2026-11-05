package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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
func timelineBurstQuery() store.BurstQuery {
	return store.BurstQuery{FromDate: "2026-10-01", ToDate: "2026-11-30", Limit: 5000}
}
func TestBurstsReturnUnsavedCorrespondents(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	id := f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), false, "")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	requirements.Len(got.Bursts, 1)
	b := got.Bursts[0]
	assertions.Equal(1, b.Inbound)
	assertions.Equal(id, b.AnchorMessageID)
	var peer store.ActivityMessageCounterpart
	for _, c := range b.Counterparts {
		if c.ParticipantID == f.peer {
			peer = c
		}
	}
	assertions.Zero(peer.PersonID)
	assertions.Equal("alice@example.com", peer.Address)
	assertions.True(peer.Counted)
}

func TestActivityExclusionsUseBoundIdentities(t *testing.T) {
	for _, tc := range []struct {
		name, address, alias, phone, kind, value, excluded string
		saved                                              []bool
		aggregate, defaultView, phoneOnly, out             bool
	}{
		{name: "human email", address: "noreply@example.com", alias: "human@example.com", aggregate: true, defaultView: true, saved: []bool{true}},
		{name: "phone", address: "noreply@example.com", phone: "+12025550123", saved: []bool{true}},
		{name: "chat identifier", address: "noreply@example.com", kind: "matrix", value: "@peer:example.com", saved: []bool{true}},
		{name: "email identifier", address: "noreply@example.com", kind: "email", value: "human@example.com", saved: []bool{true}},
		{name: "room human email", address: "room@resource.calendar.google.com", alias: "human@example.com", saved: []bool{true}},
		{name: "room phone", address: "room@resource.calendar.google.com", phone: "+12025550123", excluded: "calendar_room", saved: []bool{true}},
		{name: "automated aliases", address: "noreply@example.com", alias: "noreply@other.example.com", excluded: "automated_address", saved: []bool{false, true}},
		{name: "mixed categories", address: "noreply@example.com", alias: "human@resource.calendar.google.com", saved: []bool{true}},
		{name: "overlapping categories", address: "noreply@resource.calendar.google.com", alias: "noreply@example.com", excluded: "automated_address", saved: []bool{true}},
		{name: "phone-only participant", phone: "+14155550123", phoneOnly: true, saved: []bool{false}, out: true},
	} {
		for _, saved := range tc.saved {
			t.Run(fmt.Sprintf("%s/saved=%t", tc.name, saved), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				f := newTimelineActivityFixture(t)
				if tc.phoneOnly {
					_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address=NULL,phone_number=? WHERE id=?`), tc.phone, f.peer)
					require.NoError(err)
					require.NoError(f.Store.SetParticipantIdentifier(f.peer, "phone", tc.phone))
				} else {
					f.peer = f.EnsureParticipant(tc.address, "Peer", "example.com")
				}
				subject := store.ActivitySubject{ParticipantIDs: []int64{f.peer}}
				if saved {
					person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
					require.NoError(err)
					var alias int64
					err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO participants(email_address,phone_number,display_name) VALUES(?,?,'Alias') RETURNING id`), sql.NullString{String: tc.alias, Valid: tc.alias != ""}, sql.NullString{String: tc.phone, Valid: tc.phone != ""}).Scan(&alias)
					require.NoError(err)
					if tc.kind != "" {
						require.NoError(f.Store.SetParticipantIdentifier(alias, tc.kind, tc.value))
					}
					_, err = f.Store.LinkParticipants(f.peer, alias)
					require.NoError(err)
					subject = store.ActivitySubject{PersonID: person.ID}
				}
				q := timelineContactQuery(f)
				f.add(t, f.ConvID, f.Source.ID, q.Now, tc.out, "")
				projectTierSignals(t, f.Fixture, "UTC", 0)
				q.Subject = subject
				want := 0
				if tc.excluded == "" {
					want = 1
				}
				if tc.aggregate {
					aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
					require.NoError(err)
					assert.Equal(want, aggregate.Count)
				}
				bq := timelineBurstQuery()
				if tc.defaultView {
					page, err := f.Store.ListActivityBurstsContext(t.Context(), bq)
					require.NoError(err)
					assert.Len(page.Bursts, want)
				}
				bq.Filters.IncludeNoise = true
				page, err := f.Store.ListActivityBurstsContext(t.Context(), bq)
				require.NoError(err)
				require.Len(page.Bursts, 1)
				found := false
				for _, cp := range page.Bursts[0].Counterparts {
					if cp.ParticipantID == f.peer {
						found = true
						assert.Equal(tc.excluded, cp.Excluded)
						assert.Equal(want == 1, cp.Counted)
					}
				}
				assert.True(found, "burst includes the counterpart")
			})
		}
	}
}
func TestBurstsAggregateLargeConversationDayExactly(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	// Seed real persisted spine rows in a transaction: pagination must aggregate
	// the entire group, independently of projector batch scheduling.
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
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
		requirements.NoError(err)
		recipient := f.owner
		if i%2 == 1 {
			recipient = f.peer
		}
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')`), id, recipient)
		requirements.NoError(err)
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO activity_events(message_id,ref_kind,source_id,conversation_id,channel,occurred_at,date_origin,date_precision,timezone,utc_offset_minutes,local_date,direction,projected_last_modified,projected_identity_revision,projected_account_identity_revision) VALUES(?,'message',?,?,'email',?,'sent_at','timestamp','UTC',0,'2026-10-22',?,?,0,0)`), id, f.Source.ID, f.ConvID, at.Add(time.Duration(i)*time.Second), direction, at)
		requirements.NoError(err)
	}
	requirements.NoError(tx.Commit())
	for i := range 3 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("interleave-%d", i), "Other")
		requirements.NoError(err)
		f.add(t, conv, f.Source.ID, at.Add(time.Duration(i)*time.Minute), false, "")
	}
	// Project the three interleaved rows without replacing the large fixture.
	projectTierSignals(t, f.Fixture, "UTC", 0)
	for _, subject := range []store.ActivitySubject{{}, {ParticipantIDs: []int64{f.peer}}} {
		q := timelineBurstQuery()
		q.Limit = 2
		q.Subject = subject
		seen := map[string]bool{}
		total := 0
		for {
			page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			for _, b := range page.Bursts {
				assertions.False(seen[b.Key])
				seen[b.Key] = true
				total++
				if b.ConversationID == f.ConvID {
					assertions.Equal(3000, b.Inbound)
					assertions.Equal(3000, b.Outbound)
					assertions.Len(b.LatestMessageIDs, 5)
				}
			}
			if page.NextKey == nil {
				break
			}
			q.AfterKey = page.NextKey
		}
		assertions.Equal(4, total)
	}
}

func TestBurstsOutboundOwnerIsNotNoise(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	f.add(t, f.ConvID, f.Source.ID, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), true, "")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	requirements.Len(got.Bursts, 1)
	assertions.Equal(1, got.Bursts[0].Outbound)
	assertions.Zero(got.NoiseOmitted)
}

func TestActivityListExclusionCanBeDisabled(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	id := f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), id)
	requirements.NoError(err)
	projectTierSignals(t, f.Fixture, "UTC", 0)
	filters := store.ActivityFilters{Exclusions: persontiers.DefaultConfig()}
	filters.Exclusions.ExcludeListMessages = false
	query := timelineBurstQuery()
	query.Filters = filters
	bursts, err := f.Store.ListActivityBurstsContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(bursts.Bursts, 1)
	assertions.Equal(1, bursts.Bursts[0].Inbound)
	assertions.Zero(bursts.NoiseOmitted)
	messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: filters})
	requirements.NoError(err)
	requirements.Len(messages, 1)
	q.Filters = filters
	aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1, aggregate.Count)
	requirements.NotNil(aggregate.LastAt)
	assertions.Equal(q.Now, *aggregate.LastAt)
}

func TestActivityCallLogsNeverCountAsMessageContact(t *testing.T) {
	for _, tc := range []struct {
		kind, subject string
		want          int
	}{
		{"synctech_sms_call", "Call missed, 0 seconds", 0},
		{"synctech_sms_call", "Call rejected, 0 seconds", 0},
		{"google_voice_call", "Missed call", 0},
		{"email", "Ordinary email", 1},
		{"sms", "Ordinary text", 1},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			q := timelineContactQuery(f)
			id := f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type=?,subject=? WHERE id=?`), tc.kind, tc.subject, id)
			requirements.NoError(err)
			projectTierSignals(t, f.Fixture, "UTC", 0)
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Equal(tc.want, aggregate.Count)
			burstQuery := timelineBurstQuery()
			burstQuery.Filters.IncludeNoise = true
			bursts, err := f.Store.ListActivityBurstsContext(t.Context(), burstQuery)
			requirements.NoError(err)
			requirements.Len(bursts.Bursts, 1)
			messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
			requirements.NoError(err)
			requirements.Len(messages, 1)
			foundPeer := false
			for _, cp := range messages[0].Counterparts {
				if cp.ParticipantID == f.peer {
					foundPeer = true
					assertions.Equal(tc.want == 1, cp.Counted)
				}
			}
			assertions.True(foundPeer)
			if tc.want == 0 {
				assertions.Nil(aggregate.FirstAt)
				assertions.Nil(aggregate.LastAt)
				assertions.Nil(aggregate.LastInboundAt)
				assertions.Nil(aggregate.LastOutboundAt)
			} else {
				requirements.NotNil(aggregate.LastAt)
				assertions.Equal(q.Now, *aggregate.LastAt)
			}
		})
	}
}
