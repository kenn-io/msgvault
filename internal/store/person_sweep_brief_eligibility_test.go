package store

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonSweepBriefEligibilityReportsEnrollmentAndBoundary(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefFixture(t, "sweep-eligibility")

	_, _, err := f.store.PersonSweepBriefEligibility(t.Context(), 0)
	requirements.Error(err, "a person id is required")

	_, enrolled, err := f.store.PersonSweepBriefEligibility(t.Context(), f.personID)
	requirements.NoError(err)
	checks.False(enrolled, "a tracked but unenrolled person is not eligible")

	_, err = f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, true, "test", false)
	requirements.NoError(err)
	eligibility, enrolled, err := f.store.PersonSweepBriefEligibility(t.Context(), f.personID)
	requirements.NoError(err)
	requirements.True(enrolled)
	checks.Equal(f.personID, eligibility.PersonID)
	checks.Zero(eligibility.Version, "a person with no brief yet reports version zero")
	checks.Nil(eligibility.Boundary)

	f.applyBrief(t, f.briefInsert(7700, personBriefNow))
	eligibility, enrolled, err = f.store.PersonSweepBriefEligibility(t.Context(), f.personID)
	requirements.NoError(err)
	requirements.True(enrolled)
	checks.Equal(1, eligibility.Version)
	checks.Equal(PersonBriefStatusCurrent, eligibility.Status)
	requirements.NotNil(eligibility.GeneratedAt)
	requirements.NotNil(eligibility.Boundary)
	checks.Equal(int64(7700), eligibility.Boundary.ThroughSequence,
		"the boundary bounds the new-activity check")
	checks.Equal([]string{"conversation_text"}, eligibility.Boundary.Lanes)

	_, err = f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, false, "test", false)
	requirements.NoError(err)
	_, enrolled, err = f.store.PersonSweepBriefEligibility(t.Context(), f.personID)
	requirements.NoError(err)
	checks.False(enrolled, "unenrolling stops the scheduler from spending on the person")
}

func TestPersonSweepBriefEligibilityRejectsUnknownEventMemoryVersion(t *testing.T) {
	f := newPersonBriefFixture(t, "future-event-memory")
	_, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.personID, true, "test", false)
	require.NoError(t, err)
	input := f.briefInsert(7700, personBriefNow)
	input.Boundary = jsontext.Value(`{"event_memory_version":2,"through_sequence":7700}`)
	f.applyBrief(t, input)
	_, _, err = f.store.PersonSweepBriefEligibility(t.Context(), f.personID)
	assert.Error(t, err)
}
