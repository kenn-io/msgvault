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

var tierAsOf = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func tierOptions() store.PersonTierOptions {
	return store.TierOptionsAsOf(store.PersonTierOptions{
		Config: persontiers.DefaultConfig(), ActivityTimezone: "UTC", ActivityMaxDirect: 25,
	}, tierAsOf)
}

// seedTierMail writes one message per entry. A negative day offset is that
// many days before tierAsOf; outbound entries are sent by the owner.
func seedTierMail(t *testing.T, f *storetest.Fixture, participant int64, owner int64, mail []tierMail) {
	t.Helper()
	require := require.New(t)
	for i, m := range mail {
		from, to := participant, owner
		if m.outbound {
			from, to = owner, participant
		}
		message := f.NewMessage().
			WithSourceMessageID(fmt.Sprintf("tier-preview-%d-%d", participant, i)).
			WithSentAt(tierAsOf.AddDate(0, 0, m.day)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
	}
}

type tierMail struct {
	day      int
	outbound bool
}

// alternating returns one message a day on the given offsets, outbound on
// even positions.
func alternating(days ...int) []tierMail {
	mail := make([]tierMail, len(days))
	for i, day := range days {
		mail[i] = tierMail{day: day, outbound: i%2 == 0}
	}
	return mail
}

type tierPeople struct {
	f                                      *storetest.Fixture
	frequent, regular, oneWay, old, latest int64
}

// newTierPeople saves five people: frequent (tier 1), regular (tier 2), oneWay
// (inbound only, tier 3), old (mail only 400 days ago, tier 3) and latest
// (tier 1 with the newest contact, saved last).
func newTierPeople(t *testing.T) tierPeople {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	p := tierPeople{f: f}
	save := func(name string, mail []tierMail) int64 {
		person := mustPromotedPerson(t, f.Store, name+"@example.com", name)
		seedTierMail(t, f, person.ParticipantIDs[0], owner, mail)
		return person.ID
	}
	p.frequent = save("frequent", alternating(-1, -2, -3, -4, -5, -6, -7, -8))
	p.regular = save("regular", alternating(-100, -120, -140, -160))
	p.oneWay = save("oneway", []tierMail{{-1, false}, {-2, false}, {-3, false}, {-4, false}})
	p.old = save("old", alternating(-400, -401))
	p.latest = save("latest", alternating(0, -2, -4, -6, -8, -10, -12, -14))
	projectTierSignals(t, f, "UTC", 25)
	return p
}

func previewTiers(t *testing.T, p tierPeople) *store.PersonTierPreview {
	t.Helper()
	preview, err := p.f.Store.PreviewPersonTiersContext(t.Context(), tierOptions())
	require.NoError(t, err)
	return preview
}

// Reading events in small pages cuts a person's rows across page boundaries and
// must not change a count.
func TestPersonTierCountsDoNotDependOnReadPageSize(t *testing.T) {
	p := newTierPeople(t)
	explain := func(id int64) *store.PersonTierExplanation {
		explained, err := p.f.Store.ExplainPersonTierContext(t.Context(), id, tierOptions())
		require.NoError(t, err)
		return explained
	}
	want := []*store.PersonTierExplanation{explain(p.frequent), explain(p.latest)}
	defer store.SetPersonTierRowPage(3)()
	assert.Equal(t, want, []*store.PersonTierExplanation{explain(p.frequent), explain(p.latest)})
}

func TestPreviewPersonTiersListsTierOneAndTwoWithRules(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	p := newTierPeople(t)
	_, err := p.f.Store.SetPersonTrackingContext(t.Context(), p.regular, true)
	require.NoError(err)

	preview := previewTiers(t, p)

	assert.True(preview.Readiness.Ready)
	require.Len(preview.People, 3)
	// "latest" was saved last but has the newest contact in tier 1.
	assert.Equal(p.latest, preview.People[0].PersonID)
	frequent, regular := preview.People[1], preview.People[2]
	assert.Equal(p.frequent, frequent.PersonID)
	assert.Equal(1, frequent.Tier)
	assert.Equal("tier1_intensity", frequent.Rule)
	assert.False(frequent.Tracked)
	require.NotNil(frequent.LastContactDate)
	assert.Equal("2026-10-16", *frequent.LastContactDate)
	assert.Equal(p.regular, regular.PersonID)
	assert.Equal(2, regular.Tier)
	assert.Equal("tier2_two_way", regular.Rule)
	assert.True(regular.Tracked)
}

func TestPreviewPersonTiersReadsMoreSavedPeopleThanOnePage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	// Bare profiles with no participants fill the first page.
	for i := range 500 {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO persons (vcard_uid) VALUES (?)`), fmt.Sprintf("bulk-%d", i))
		require.NoError(err)
	}
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	// The last saved person sits on the second page of 500.
	late := mustPromotedPerson(t, f.Store, "late@example.com", "late")
	seedTierMail(t, f, late.ParticipantIDs[0], owner, alternating(-1, -2, -3, -4, -5, -6, -7, -8))
	projectTierSignals(t, f, "UTC", 25)
	preview, err := f.Store.PreviewPersonTiersContext(t.Context(), tierOptions())
	require.NoError(err)
	require.Len(preview.People, 1)
	assert.Equal(late.ID, preview.People[0].PersonID)
}

func TestExplainPersonTierShowsCountsAgainstThresholds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	p := newTierPeople(t)
	explanation, err := p.f.Store.ExplainPersonTierContext(t.Context(), p.regular, tierOptions())
	require.NoError(err)
	require.NotNil(explanation.LastContactDate)
	assert.Equal("2026-07-09", *explanation.LastContactDate)
	assert.Contains(explanation.Thresholds, persontiers.ThresholdCheck{
		Name: "tier2.min_active_days_long", Observed: 4, Required: 3, Enabled: true, Met: true,
	})
}

func TestExplainPersonTierRejectsUnknownPeople(t *testing.T) {
	p := newTierPeople(t)
	_, err := p.f.Store.ExplainPersonTierContext(t.Context(), 99999, tierOptions())
	require.ErrorIs(t, err, store.ErrPersonNotFound)
}

func TestPersonTiersFollowTheStoredProjectionNotTheConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	a := newAudienceFixture(t)
	x := a.f.EnsureParticipant("x@example.com", "X", "example.com")
	y := a.f.EnsureParticipant("y@example.com", "Y", "example.com")
	a.message(t, "stored-limit", -1, a.f.ConvID, "", a.owner, x, y)
	projectTierSignals(t, a.f, "UTC", 25)
	options := tierOptions()

	explanation, err := a.f.Store.ExplainPersonTierContext(t.Context(), a.personID, options)
	require.NoError(err)
	assert.True(explanation.Readiness.Ready)

	options.ActivityMaxDirect = 1
	explanation, err = a.f.Store.ExplainPersonTierContext(t.Context(), a.personID, options)
	require.NoError(err)
	assert.Equal(store.PersonTierProjection{Timezone: "UTC", MaxDirect: 25}, explanation.Projection)
	assert.False(explanation.Readiness.Ready)
	require.Len(explanation.Readiness.Reasons, 1)
	assert.Contains(explanation.Readiness.Reasons[0], "settings_changed: stored timezone UTC and direct limit 25, configured UTC and 1")
	assert.Equal(1, explanation.Signals.InboundLong, "a limit of 1 would have dropped this message")
	assert.Zero(explanation.Signals.LargeAudienceInboundEvents)
}

func TestPreviewPersonTiersBeforeAnyProjectionUsesDefaults(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	options := tierOptions()
	options.ActivityTimezone, options.ActivityMaxDirect = "America/New_York", 10

	preview, err := f.Store.PreviewPersonTiersContext(t.Context(), options)

	require.NoError(err)
	assert.Equal(store.PersonTierProjection{Timezone: "UTC", MaxDirect: 25}, preview.Projection)
	assert.NotContains(preview.Readiness.Reasons, "watermark_behind")
	for _, reason := range preview.Readiness.Reasons {
		assert.NotContains(reason, "settings_changed", "nothing is stored to differ from")
	}
	assert.Empty(preview.People)
}

// While the projector replays under a new setting, the stored rows follow
// neither value. A preview then would drop people from their tier, so it
// refuses until the replay finishes.
func TestPersonTiersRefuseWhileTheProjectionRebuilds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	p := newTierPeople(t)
	_, err := p.f.Store.ClaimActivityTimezoneTransitionContext(t.Context(), "America/New_York")
	require.NoError(err)
	_, err = p.f.Store.PreviewPersonTiersContext(t.Context(), tierOptions())
	require.ErrorIs(err, store.ErrPersonTierRebuilding)
	_, err = p.f.Store.ExplainPersonTierContext(t.Context(), p.frequent, tierOptions())
	require.ErrorIs(err, store.ErrPersonTierRebuilding)
	projectTierSignals(t, p.f, "America/New_York", 25)
	after := previewTiers(t, p)
	assert.Equal("America/New_York", after.Projection.Timezone)
	assert.Len(after.People, 3, "the same people are listed once the rebuild finishes")

	_, err = p.f.Store.ClaimActivityDirectLimitTransitionContext(t.Context(), 10)
	require.NoError(err)
	_, err = p.f.Store.PreviewPersonTiersContext(t.Context(), tierOptions())
	require.ErrorIs(err, store.ErrPersonTierRebuilding)
	projectTierSignals(t, p.f, "America/New_York", 10)
	assert.Equal(10, previewTiers(t, p).Projection.MaxDirect)
}

// Dedup can keep draft evidence on a message that was sent. The projector lets
// the Sent label win, and the tier counts must follow it.
func TestPersonTiersCountSentMailThatKeptDraftEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	person := mustPromotedPerson(t, f.Store, "sent@example.com", "sent")
	seedTierMail(t, f, person.ParticipantIDs[0], owner, alternating(-1, -2, -3, -4, -5, -6, -7, -8))
	labels, err := f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{
		"Drafts": {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
		"Sent":   {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	rows, err := f.Store.DB().QueryContext(t.Context(), f.Store.Rebind(`SELECT id FROM messages WHERE sender_id = ?`), owner)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var sent []int64
	for rows.Next() {
		var id int64
		require.NoError(rows.Scan(&id))
		sent = append(sent, id)
	}
	require.NoError(rows.Err())
	require.Len(sent, 4)
	for _, id := range sent {
		require.NoError(f.Store.AddMessageLabels(id, []int64{labels["Drafts"], labels["Sent"]}))
	}
	projectTierSignals(t, f, "UTC", 25)

	explanation, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID, tierOptions())
	require.NoError(err)
	assert.Equal(4, explanation.Signals.OutboundShort)
	assert.Equal(1, explanation.Tier)
}

func TestPersonTiersRejectInvalidRules(t *testing.T) {
	require := require.New(t)
	p := newTierPeople(t)
	options := tierOptions()
	options.Config.Tier1.MinOutboundShort = 0
	_, err := p.f.Store.PreviewPersonTiersContext(t.Context(), options)
	require.ErrorIs(err, persontiers.ErrInvalidConfig)
	_, err = p.f.Store.ExplainPersonTierContext(t.Context(), p.frequent, options)
	require.ErrorIs(err, persontiers.ErrInvalidConfig)
}
