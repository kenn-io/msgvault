package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

var personBriefNow = time.Date(2026, time.September, 1, 9, 0, 0, 0, time.UTC)

// personBriefFixture is one tracked person plus a fact generation and the
// evidence rows a brief can point at. Everything a brief needs already exists
// on the sweep path, so the fixture builds it through that path rather than
// inserting rows by hand.
type personBriefFixture struct {
	store        *Store
	personID     int64
	generationID int64
	evidenceIDs  []int64
	evidenceKeys []string
}

func newPersonBriefFixture(t *testing.T, suffix string) personBriefFixture {
	t.Helper()
	st, personID := newPersonFactLedgerStore(t)
	_, err := st.SetPersonTrackingContext(t.Context(), personID, true)
	require.NoError(t, err)
	prepared := preparePersonFactLedgerGeneration(t, personID, "brief-"+suffix,
		[]personfacts.ProposedClaim{
			personFactLedgerClaim(personID, "brief-target-a-"+suffix, `"value-a"`, "brief-a-"+suffix),
			personFactLedgerClaim(personID, "brief-target-b-"+suffix, `"value-b"`, "brief-b-"+suffix),
		}, nil)
	stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	require.Len(t, stored.Evidence, 2)
	fixture := personBriefFixture{
		store: st, personID: personID, generationID: stored.Generation.ID,
	}
	for _, evidence := range stored.Evidence {
		fixture.evidenceIDs = append(fixture.evidenceIDs, evidence.ID)
		fixture.evidenceKeys = append(fixture.evidenceKeys, evidence.Key)
	}
	return fixture
}

func (f personBriefFixture) briefInsert(throughSequence int64, generatedAt time.Time) PersonBriefInsert {
	boundary, err := json.Marshal(map[string]any{
		"lanes":              []string{"conversation_text"},
		"through_sequence":   throughSequence,
		"item_count":         2,
		"through_event_time": generatedAt.Format(time.RFC3339),
	})
	if err != nil {
		panic(err)
	}
	return PersonBriefInsert{
		PersonID: f.personID, GenerationID: f.generationID,
		ProgramID: "msgvault-person-brief", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("b", 64),
		Provider:           "fixture", ProviderVersion: "v1",
		Model: "fixture-model", ModelVersion: "v1",
		ProviderPolicyFingerprint: "policy-v1",
		Boundary:                  boundary,
		Structured:                json.RawMessage(`{"highlights":[{"text":"synthetic"}]}`),
		RenderedText:              "Last time you talked (Sep 1): synthetic.",
		RendererPolicy:            "person-brief-render-v1",
		DroppedItemCount:          1,
		GeneratedAt:               generatedAt,
		EvidenceIDs:               f.evidenceIDs,
	}
}

func (f personBriefFixture) applyBrief(t *testing.T, input PersonBriefInsert) PersonBrief {
	t.Helper()
	var brief PersonBrief
	err := f.store.withTxContext(t.Context(), func(tx *loggedTx) error {
		var applyErr error
		brief, applyErr = f.store.applyPersonBriefTx(t.Context(), tx, input)
		return applyErr
	})
	require.NoError(t, err)
	return brief
}

func TestPersonBriefEnrollmentRequiresTracking(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID := newPersonFactLedgerStore(t)

	_, err := st.SetPersonBriefEnrollmentContext(t.Context(), personID, true, "owner", false)
	require.Error(err)
	require.ErrorIs(err, ErrPersonBriefNotTracked)
	assert.Contains(err.Error(), "msgvault person track",
		"the refusal must name the command that fixes it")
	enrollment, err := st.GetPersonBriefEnrollmentContext(t.Context(), personID)
	require.NoError(err)
	assert.False(enrollment.Enrolled)
	tracking, err := st.GetPersonTrackingContext(t.Context(), personID)
	require.NoError(err)
	assert.False(tracking.Tracked, "a refused enrollment must not track the person")

	enrollment, err = st.SetPersonBriefEnrollmentContext(t.Context(), personID, true, "owner", true)
	require.NoError(err)
	assert.True(enrollment.Enrolled)
	assert.Equal("owner", enrollment.Actor)
	tracking, err = st.GetPersonTrackingContext(t.Context(), personID)
	require.NoError(err)
	assert.True(tracking.Tracked, "--track must create the tracking row in the same transaction")
}

func TestPersonBriefEnrollmentIsIdempotentAndReversible(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "enrollment")

	first, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, true, "owner", false)
	require.NoError(err)
	require.True(first.Enrolled)
	second, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, true, "other", false)
	require.NoError(err)
	assert.True(second.Enrolled)
	assert.Equal(first.EnabledAt, second.EnabledAt, "re-enrolling must not rewrite the enrollment")
	assert.Equal("owner", second.Actor)

	brief := f.applyBrief(t, f.briefInsert(1000, personBriefNow))
	off, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, false, "owner", false)
	require.NoError(err)
	assert.False(off.Enrolled)
	assert.Nil(off.EnabledAt)

	readable, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	require.NoError(err)
	assert.Equal(brief.ID, readable.ID, "unenrolling must leave existing versions readable")
}

func TestPersonBriefEnrollmentListIsBoundedAndAscending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "list")
	_, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, true, "owner", false)
	require.NoError(err)
	second, err := f.store.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	bob, _, err := f.store.CreatePersonFromParticipant(second)
	require.NoError(err)
	_, err = f.store.SetPersonBriefEnrollmentContext(t.Context(), bob.ID, true, "owner", true)
	require.NoError(err)

	page, err := f.store.ListPersonBriefEnrollmentsContext(t.Context(), 0, 1)
	require.NoError(err)
	require.Len(page, 1)
	assert.Equal(min(f.personID, bob.ID), page[0].PersonID)

	rest, err := f.store.ListPersonBriefEnrollmentsContext(t.Context(), page[0].PersonID, 10)
	require.NoError(err)
	require.Len(rest, 1)
	assert.Equal(max(f.personID, bob.ID), rest[0].PersonID)

	_, err = f.store.ListPersonBriefEnrollmentsContext(t.Context(), 0, 0)
	assert.Error(err, "an unbounded listing must be refused")
}

func TestPersonBriefVersionsAreImmutableAndSupersede(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "versions")

	first := f.applyBrief(t, f.briefInsert(1000, personBriefNow))
	assert.Equal(1, first.Version)
	assert.Equal(PersonBriefStatusCurrent, first.Status)
	assert.Nil(first.SupersededAt)
	assert.Equal(1, first.DroppedItemCount)

	regenerated := f.briefInsert(2000, personBriefNow.Add(time.Hour))
	regenerated.Structured = json.RawMessage(`{"highlights":[{"text":"second"}]}`)
	regenerated.RenderedText = "Last time you talked (Sep 1): second."
	second := f.applyBrief(t, regenerated)
	assert.Equal(2, second.Version)
	assert.Equal(PersonBriefStatusCurrent, second.Status)

	stale, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 1)
	require.NoError(err)
	assert.Equal(PersonBriefStatusSuperseded, stale.Status)
	require.NotNil(stale.SupersededAt)
	assert.JSONEq(string(first.Structured), string(stale.Structured),
		"a superseded version keeps the structure the owner saw")
	assert.Equal(first.RenderedText, stale.RenderedText)

	current, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	require.NoError(err)
	assert.Equal(second.ID, current.ID)

	versions, err := f.store.ListPersonBriefVersionsContext(t.Context(), f.personID, 10)
	require.NoError(err)
	require.Len(versions, 2)
	assert.Equal(2, versions[0].Version)
	assert.Equal(1, versions[1].Version)
}

func TestPersonBriefKeepsOneCurrentVersionPerPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "one-current")
	first := f.applyBrief(t, f.briefInsert(1000, personBriefNow))

	_, err := f.store.db.ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_briefs
			(person_id, version, generation_id, status, program_id, program_version,
			 program_fingerprint, provider, provider_version, model, model_version,
			 provider_policy_fingerprint, boundary_json, structured_json, rendered_text,
			 renderer_policy, generated_at)
		VALUES (?, 99, ?, 'current', 'p', 'v1', ?, 'provider', 'v1', 'model', 'v1',
			'policy', `+f.store.dialect.JSONBindExpr()+`, `+f.store.dialect.JSONBindExpr()+`,
			'text', 'person-brief-render-v1', ?)`),
		f.personID, f.generationID, strings.Repeat("b", 64), `{}`, `{}`, personBriefNow)
	require.Error(err, "a second current version must violate the partial unique index")

	current, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	require.NoError(err)
	assert.Equal(first.ID, current.ID)
}

func TestPersonBriefRejectionAppliesToCurrentVersionOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "reject")
	f.applyBrief(t, f.briefInsert(1000, personBriefNow))
	rejectedAt := personBriefNow.Add(2 * time.Hour)

	rejected, err := f.store.RejectPersonBriefContext(t.Context(), f.personID, "wrong person", rejectedAt)
	require.NoError(err)
	assert.Equal(PersonBriefStatusRejected, rejected.Status)
	assert.Equal("wrong person", rejected.RejectedReason)
	require.NotNil(rejected.RejectedAt)
	assert.True(rejectedAt.Equal(*rejected.RejectedAt))

	_, err = f.store.RejectPersonBriefContext(t.Context(), f.personID, "again", rejectedAt)
	require.ErrorIs(err, ErrPersonBriefNotFound, "only a current version can be rejected")
	_, err = f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	require.ErrorIs(err, ErrPersonBriefNotFound)

	next := f.applyBrief(t, f.briefInsert(3000, personBriefNow.Add(3*time.Hour)))
	assert.Equal(2, next.Version, "the next regeneration takes the next version")
	history, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 1)
	require.NoError(err)
	assert.Equal(PersonBriefStatusRejected, history.Status,
		"a rejected version stays readable in history")
}

func TestPersonBriefEvidencePointersReportSupportFromStatusEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "evidence")
	brief := f.applyBrief(t, f.briefInsert(1000, personBriefNow))

	pointers, err := f.store.ListPersonBriefEvidenceContext(t.Context(), brief.ID)
	require.NoError(err)
	require.Len(pointers, 2)
	assert.Equal(0, pointers[0].Ordinal)
	assert.Equal(1, pointers[1].Ordinal)
	storedEvidenceIDs := []int64{pointers[0].EvidenceID, pointers[1].EvidenceID}
	assert.Equal(f.evidenceIDs, storedEvidenceIDs)
	assert.True(pointers[0].Supported)
	assert.True(pointers[1].Supported)

	invalidation := preparePersonFactLedgerGeneration(t, f.personID, "brief-invalidate", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: f.evidenceKeys[0], SourceVersion: "source-v1",
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	persistPersonFactLedgerGeneration(t, f.store, invalidation, nil)

	pointers, err = f.store.ListPersonBriefEvidenceContext(t.Context(), brief.ID)
	require.NoError(err)
	require.Len(pointers, 2)
	assert.False(pointers[0].Supported, "an invalidated source marks the pointer unsupported")
	assert.True(pointers[1].Supported)
}

func TestListBriefEligiblePeopleReportsCurrentVersionMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "eligible")

	eligible, err := f.store.ListBriefEligiblePeopleContext(t.Context(), 0, 10)
	require.NoError(err)
	assert.Empty(eligible, "an unenrolled person is not brief eligible")

	_, err = f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, true, "owner", false)
	require.NoError(err)
	eligible, err = f.store.ListBriefEligiblePeopleContext(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(eligible, 1)
	assert.Equal(f.personID, eligible[0].PersonID)
	assert.Zero(eligible[0].Version, "a person with no brief yet reports no version")

	f.applyBrief(t, f.briefInsert(184233, personBriefNow))
	eligible, err = f.store.ListBriefEligiblePeopleContext(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(eligible, 1)
	assert.Equal(1, eligible[0].Version)
	assert.Equal(PersonBriefStatusCurrent, eligible[0].Status)
	assert.Equal(int64(184233), eligible[0].ThroughSequence)
	require.NotNil(eligible[0].GeneratedAt)
	assert.True(personBriefNow.Equal(*eligible[0].GeneratedAt))

	_, err = f.store.SetPersonTrackingContext(t.Context(), f.personID, false)
	require.NoError(err)
	eligible, err = f.store.ListBriefEligiblePeopleContext(t.Context(), 0, 10)
	require.NoError(err)
	assert.Empty(eligible, "an untracked person is not brief eligible")
}

func TestApplyPersonBriefRejectsInconsistentInput(t *testing.T) {
	f := newPersonBriefFixture(t, "validation")

	for _, test := range []struct {
		name   string
		mutate func(*PersonBriefInsert)
	}{
		{name: "missing renderer policy", mutate: func(i *PersonBriefInsert) { i.RendererPolicy = "" }},
		{name: "malformed structure", mutate: func(i *PersonBriefInsert) {
			i.Structured = json.RawMessage(`{"broken"`)
		}},
		{name: "malformed boundary", mutate: func(i *PersonBriefInsert) {
			i.Boundary = json.RawMessage(`not json`)
		}},
		{name: "foreign person", mutate: func(i *PersonBriefInsert) { i.PersonID = f.personID + 10_000 }},
		{name: "duplicate evidence", mutate: func(i *PersonBriefInsert) {
			i.EvidenceIDs = []int64{f.evidenceIDs[0], f.evidenceIDs[0]}
		}},
		{name: "unknown evidence", mutate: func(i *PersonBriefInsert) {
			i.EvidenceIDs = []int64{f.evidenceIDs[0] + 100_000}
		}},
		{name: "zero generated at", mutate: func(i *PersonBriefInsert) { i.GeneratedAt = time.Time{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := f.briefInsert(1000, personBriefNow)
			test.mutate(&input)
			err := f.store.withTxContext(t.Context(), func(tx *loggedTx) error {
				_, applyErr := f.store.applyPersonBriefTx(t.Context(), tx, input)
				return applyErr
			})
			require.Error(t, err)
		})
	}

	var briefs int
	require.NoError(t, f.store.db.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_briefs WHERE person_id = ?`), f.personID).Scan(&briefs))
	assert.Zero(t, briefs, "no rejected input may leave a partial version behind")
}

func TestPersonBriefReadsReportMissingVersions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "missing")

	_, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	require.ErrorIs(err, ErrPersonBriefNotFound)
	_, err = f.store.GetPersonBriefContext(t.Context(), f.personID, 7)
	require.ErrorIs(err, ErrPersonBriefNotFound)
	_, err = f.store.RejectPersonBriefContext(t.Context(), f.personID, "none", personBriefNow)
	require.ErrorIs(err, ErrPersonBriefNotFound)

	// A person with no versions lists an empty history rather than failing:
	// only a request for a specific missing version is an error.
	versions, err := f.store.ListPersonBriefVersionsContext(t.Context(), f.personID, 10)
	require.NoError(err)
	assert.Empty(versions)
}

func TestPersonBriefVersionsCascadeWithTheAbsorbedPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonBriefFixture(t, "merge")
	absorbedBrief := f.applyBrief(t, f.briefInsert(1000, personBriefNow))

	survivorParticipant, err := f.store.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	survivor, _, err := f.store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, err := f.store.GetPersonContext(t.Context(), f.personID)
	require.NoError(err)

	merged, err := f.store.MergePersonsContext(t.Context(), PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "brief-versions-merge", Actor: "test",
	})
	require.NoError(err)

	// Brief versions belong to the fact generation that produced them, and that
	// generation is scoped to the identity that produced it, so both cascade
	// with the absorbed root rather than becoming survivor profile state.
	var surviving int
	require.NoError(f.store.db.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_briefs WHERE id = ?`), absorbedBrief.ID).Scan(&surviving))
	assert.Zero(surviving)
	var pointers int
	require.NoError(f.store.db.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_brief_evidence WHERE brief_id = ?`),
		absorbedBrief.ID).Scan(&pointers))
	assert.Zero(pointers, "evidence pointers cascade with their version")
	_, err = f.store.GetPersonBriefContext(t.Context(), merged.Person.ID, 0)
	assert.ErrorIs(err, ErrPersonBriefNotFound)
}
