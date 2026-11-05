package store_test

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonTierSignalsWindowsRolesAndMeetingDays(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	drafts, err := f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{"DRAFT": {Name: "DRAFT", Type: "system"}})
	require.NoError(err)
	for i, event := range []struct {
		days                           int
		outbound, meeting, list, draft bool
	}{
		{0, true, false, false, false}, {0, false, false, false, false}, {-364, false, false, false, false},
		{-365, true, false, false, false}, {1, false, false, false, false}, {-2, false, false, true, false},
		{-3, true, true, false, false}, {-3, false, true, false, false}, {-4, true, false, false, true},
	} {
		from, to := counterpart, owner
		if event.outbound {
			from, to = owner, counterpart
		}
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-window-%d", i)).WithSentAt(asOf.AddDate(0, 0, event.days)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		if event.meeting {
			message.MessageType = "calendar_event"
		}
		if event.list {
			message.ListID = sql.NullString{String: "announcements.example.com", Valid: true}
		}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		if event.draft {
			require.NoError(f.Store.ReplaceMessageLabels(id, []int64{drafts["DRAFT"]}))
		}
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, []string{"Synthetic sender"}))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, []string{"Synthetic recipient"}))
	}
	projectTierSignals(t, f, "UTC", 25)
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	s := signals
	assert.Equal(1, s.OutboundShort)
	assert.Equal(1, s.OutboundLong)
	assert.Equal(1, s.InboundShort)
	assert.Equal(2, s.InboundLong)
	assert.Equal(2, s.ActiveDaysShort)
	assert.Equal(3, s.ActiveDaysLong)
	assert.Equal(1, s.MeetingDaysLong)
	assert.Equal(1, s.ChannelsLong, "a meeting day is not a message channel")
	assert.Equal(1, s.ExcludedListEvents)
}

func TestPersonTierSignalsUseLocalDates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var sender int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&sender))
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(err)
	asOf := time.Date(2026, 11, 2, 0, 0, 0, 0, zone)
	for i, days := range []int{-89, -90} {
		at := asOf.AddDate(0, 0, days).Add(23*time.Hour + 30*time.Minute)
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-local-%d", i)).WithSentAt(at).Build()
		message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
	}
	projectTierSignals(t, f, zone.String(), 25)
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	assert.Equal(1, signals.InboundShort)
	assert.Equal(2, signals.InboundLong)
}

// tierSignals reads one person's counts through explain.
func tierSignals(t *testing.T, f *storetest.Fixture, personID int64, asOf time.Time, cfg persontiers.Config) persontiers.Signals {
	t.Helper()
	explanation, err := f.Store.ExplainPersonTierContext(t.Context(), personID, store.TierOptionsAsOf(store.PersonTierOptions{Config: cfg}, asOf))
	require.NoError(t, err)
	return explanation.Signals
}

func tierReadiness(t *testing.T, f *storetest.Fixture) store.PersonTierReadiness {
	t.Helper()
	preview, err := f.Store.PreviewPersonTiersContext(t.Context(), tierOptions())
	require.NoError(t, err)
	return preview.Readiness
}

func projectTierSignals(t *testing.T, f *storetest.Fixture, zone string, maxDirect int) {
	t.Helper()
	require := require.New(t)
	p, err := activity.NewProjector(f.Store, activity.Options{Timezone: zone, BatchSize: 500, MaxDirectCounterparts: maxDirect})
	require.NoError(err)
	_, err = p.RunOnce(t.Context())
	require.NoError(err)
}

// Co-authors are senders, not people the owner addressed. Inbound cc and
// owner-absent mail must not count toward any tier, and a list message in a
// role no tier counts is not an excluded list message either.
func TestPersonTierSignalsRejectCoauthorsCCAndObserved(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	stranger := f.EnsureParticipant("stranger@example.com", "Stranger", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("tier-roles-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.ListID = sql.NullString{String: "announcements.example.com", Valid: true}
		from, to := []int64{stranger}, []int64{owner}
		if i == 0 {
			from, to = []int64{owner, counterpart}, []int64{stranger}
		}
		if i == 2 {
			to = []int64{stranger}
		}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", from, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
		if i > 0 {
			require.NoError(f.Store.ReplaceMessageRecipients(id, "cc", []int64{counterpart}, nil))
		}
	}
	projectTierSignals(t, f, "UTC", 25)
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	s := signals
	assert.Zero(s.OutboundLong)
	assert.Zero(s.InboundLong)
	assert.Zero(s.ActiveDaysLong)
	assert.Zero(s.ChannelsLong)
	assert.Zero(s.ExcludedListEvents)
}

func TestPersonTierSignalsBindingsFeedExclusions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{asOf.AddDate(0, 0, -400)})
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	s := signals
	assert.Zero(s.InboundLong)
	assert.Equal([]string{"counterpart@example.com"}, s.Emails)
	assert.False(s.HasPhoneOrChat)
	assert.False(s.Owner)
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO participant_identifiers(participant_id,identifier_type,identifier_value) VALUES (?,'beeper',?)`), counterpart, "synthetic-chat-owner")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "synthetic-chat-owner", "manual"))
	signals = tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	assert.True(signals.HasPhoneOrChat)
	assert.True(signals.Owner)
}

func TestPersonTierReadiness(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	seedProjectedActivity(t, f, personID, []time.Time{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	f.CreateMessage("pending-tier-projection")
	ready := tierReadiness(t, f)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "watermark_behind")
	projectTierSignals(t, f, "UTC", 25)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "second-owner@example.com", "manual"))
	ready = tierReadiness(t, f)
	assert.False(ready.Ready)
	assert.Contains(ready.Reasons, "projection_pending")
	projectTierSignals(t, f, "UTC", 25)
	id := f.CreateMessage("tier-deleted")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	require.NoError(err)
	projectTierSignals(t, f, "UTC", 25)
	// Model a restored watermark ending at the last live message. A later deleted
	// row must not keep an otherwise reconciled archive permanently unready.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = 'activity_spine_watermark'`), strconv.FormatInt(id-1, 10))
	require.NoError(err)
	ready = tierReadiness(t, f)
	assert.True(ready.Ready)
	assert.Empty(ready.Reasons)
}

func TestPersonTierSignalsOwnerBoundThroughSecondParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{asOf})
	_, err := f.Store.LinkParticipants(counterpart, owner)
	require.NoError(err)
	projectTierSignals(t, f, "UTC", 25)
	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())
	assert.True(signals.Owner)
	assert.Zero(signals.InboundLong)
	assert.ElementsMatch([]string{"owner@example.com", "counterpart@example.com"}, signals.Emails)
}

// The projection drops the owner's own addresses only within a message's
// account. A person who is your address in another account is still excluded.
func TestPersonTierSignalsOwnerAddressInAnotherAccountIsExcluded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := newActivityQueryFixture(t)
	other, err := f.Store.GetOrCreateSource("gmail", "other-account@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(other.ID, "counterpart@example.com", "manual"))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedProjectedActivity(t, f, personID, []time.Time{asOf})

	signals := tierSignals(t, f, personID, asOf, persontiers.DefaultConfig())

	assert.Equal(1, signals.InboundLong, "this account's projection still counts the person")
	assert.True(signals.Owner)
}

func TestPersonTierSignalsCopyQualificationControls(t *testing.T) {
	for _, tc := range []struct {
		name, kind               string
		list, broadcast, deleted bool
		want                     int
	}{
		{name: "email", kind: "email_thread", want: 1},
		{name: "independent chat", kind: "chat", want: 2},
		{name: "list first", kind: "email_thread", list: true, want: 1},
		{name: "large audience first", kind: "email_thread", broadcast: true, want: 1},
		{name: "deleted first", kind: "email_thread", deleted: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			a := newAudienceFixture(t)
			room, err := a.f.Store.EnsureConversationWithType(a.f.Source.ID, "copy-controls", tc.kind, "Copy controls")
			require.NoError(err)
			guest := a.f.EnsureParticipant("guest@example.com", "Guest", "example.com")
			var firstID int64
			for i := range 2 {
				m := a.f.NewMessage().WithSentAt(a.asOf.AddDate(0, 0, -2+i)).Build()
				m.ConversationID = room
				m.SenderID = sql.NullInt64{Int64: a.sender, Valid: true}
				key := "<copy-control@example.com>"
				m.RFC822MessageID = sql.NullString{String: key, Valid: true}
				if tc.list && i == 0 {
					m.ListID = sql.NullString{String: "list.example.com", Valid: true}
				}
				id, err := a.f.Store.UpsertMessage(m)
				require.NoError(err)
				to := []int64{a.owner}
				if tc.broadcast && i == 0 {
					to = append(to, guest)
				}
				require.NoError(a.f.Store.ReplaceMessageRecipients(id, "to", to, nil))
				if i == 0 {
					firstID = id
				}
			}
			projectTierSignals(t, a.f, "UTC", 1)
			if tc.deleted {
				var projected int
				require.NoError(a.f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_events`).Scan(&projected))
				require.Equal(2, projected, "both copies must exist in the projection before deletion")
				_, err = a.f.Store.DB().ExecContext(t.Context(), a.f.Store.Rebind(`UPDATE messages SET deleted_at=? WHERE id=?`), a.asOf, firstID)
				require.NoError(err)
			}
			defer store.SetPersonTierRowPage(1)()
			signals := tierSignals(t, a.f, a.personID, a.asOf, persontiers.DefaultConfig())
			assert.Equal(tc.want, signals.InboundShort)
			if tc.deleted {
				assert.Equal(a.asOf.AddDate(0, 0, -1).Format("2006-01-02"), signals.LastDate)
			}
			if tc.list {
				assert.Equal(1, signals.ExcludedListEvents)
			}
			if tc.broadcast {
				assert.Equal(1, signals.LargeAudienceInboundEvents)
			}
		})
	}
}

// Duplicate mailbox copies must not supply the second outbound event needed
// for tier one; qualification and authorization precede logical-email collapse.
func TestPersonTierSignalsCanonicalEmailCopies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ids         [2]string
		sameMailbox bool
		days, start int
		want        [4]int // all sources, first mailbox, second mailbox, no sources
	}{
		{"six days of copies", [2]string{}, false, 6, -2, [4]int{5, 5, 5, 0}},
		{"clean brackets", [2]string{"<canonical@example.com>", "canonical@example.com"}, false, 1, -2, [4]int{1, 1, 1, 0}},
		{"short-window boundary", [2]string{"<boundary@example.com>", "boundary@example.com"}, false, 1, -90, [4]int{1, 0, 1, 0}},
		{"missing IDs", [2]string{"", ""}, true, 1, -2, [4]int{2, 2, 0, 0}},
		{"distinct IDs", [2]string{"first@example.com", "second@example.com"}, true, 1, -2, [4]int{2, 2, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			a := newAudienceFixture(t)
			second, err := a.f.Store.GetOrCreateSource("gmail", "second@example.com")
			requirements.NoError(err)
			requirements.NoError(a.f.Store.AddAccountIdentity(second.ID, "owner@example.com", "manual"))
			room, err := a.f.Store.EnsureConversationWithType(second.ID, "canonical-copies", "email_thread", "Canonical copies")
			requirements.NoError(err)
			sources := []struct{ id, room int64 }{{a.f.Source.ID, a.f.ConvID}, {second.ID, room}}
			if tc.sameMailbox {
				sources[1] = sources[0]
			}
			for day := range tc.days {
				for i, source := range sources {
					from, to := a.sender, a.owner
					at := a.asOf.AddDate(0, 0, tc.start+i)
					key := tc.ids[i]
					if tc.days > 1 {
						at = a.asOf.AddDate(0, 0, -day)
						key = fmt.Sprintf("<logical-%d@example.com>", day)
						if day == 0 {
							from, to = a.owner, a.sender
						}
					}
					m := a.f.NewMessage().WithSentAt(at).Build()
					m.SourceID, m.ConversationID = source.id, source.room
					m.SenderID = sql.NullInt64{Int64: from, Valid: true}
					m.RFC822MessageID = sql.NullString{String: key, Valid: key != ""}
					id, err := a.f.Store.UpsertMessage(m)
					requirements.NoError(err)
					requirements.NoError(a.f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
					requirements.NoError(a.f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
				}
			}
			projectTierSignals(t, a.f, "UTC", 25)
			defer store.SetPersonTierRowPage(1)()
			for i, ids := range [][]int64{nil, {a.f.Source.ID}, {second.ID}, {}} {
				ctx := t.Context()
				if ids != nil {
					ctx = sourcescope.WithSourceIDs(ctx, ids)
				}
				if tc.days > 1 {
					preview, err := a.f.Store.PreviewPersonTiersContext(ctx, tierOptions())
					requirements.NoError(err)
					if ids != nil && len(ids) == 0 {
						assertions.Empty(preview.People)
						continue
					}
					requirements.Len(preview.People, 1)
					assertions.Equal(2, preview.People[0].Tier)
				}
				explanation, err := a.f.Store.ExplainPersonTierContext(ctx, a.personID, tierOptions())
				requirements.NoError(err)
				assertions.Equal(tc.want[i], explanation.Signals.InboundShort)
				if tc.start == -90 && ids == nil {
					assertions.Equal("2026-07-20", explanation.Signals.LastDate)
				}
				if tc.days > 1 {
					assertions.Equal(1, explanation.Signals.OutboundShort)
					assertions.Equal(6, explanation.Signals.ActiveDaysShort)
				}
			}
		})
	}
}
