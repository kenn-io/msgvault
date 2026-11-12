package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestRenderHowWeMet(t *testing.T) {
	at := time.Date(2021, 4, 1, 0, 30, 0, 0, time.FixedZone("east", 3600))
	first := &firstAppearance{event: ActivityEvent{OccurredAt: at}, candidate: ActivityCandidate{ConversationType: "email_thread"}}
	for _, tc := range []struct {
		kind FirstContactKind
		want string
	}{
		{FirstContactEmailIntro, "Someone introduced them by email in March 2021."},
		{FirstContactOwnerOutreach, "You contacted them by email in March 2021."},
		{FirstContactInboundOutreach, "They contacted you by email in March 2021."},
		{FirstContactGroupShared, "You shared a group conversation in March 2021."},
		{FirstContactMeeting, "You shared a meeting in March 2021."},
		{FirstContactObserved, ""},
	} {
		t.Run(string(tc.kind), func(t *testing.T) { first.kind = tc.kind; assert.Equal(t, tc.want, renderHowWeMet(first)) })
	}
	first.kind = FirstContactEmailIntro
	first.introducer = &FirstContactIntroducer{DisplayName: strings.Repeat("雪", 500)}
	result := renderHowWeMet(first)
	assert.LessOrEqual(t, len([]rune(result)), 280)
	assert.Equal(t, strings.Repeat("雪", 120)+" introduced them by email in March 2021.", result)
	assert.Empty(t, renderHowWeMet(nil))
	first.candidate.ConversationType = "direct_chat"
	first.kind = FirstContactOwnerOutreach
	assert.Equal(t, "You contacted them by chat in March 2021.", renderHowWeMet(first))
}

func TestHowWeMetUnownedProjectionPinAndUnpin(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugHowWeMet)
	from := personFactLedgerNow.Add(-24 * time.Hour)
	ref := "first-appearance:email_intro:123"
	_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugHowWeMet, Source: ProvenanceSystem,
		SourceRef: &ref, ActiveFrom: &from,
		Value: AttributeValue{Type: AttributeValueText, Text: new("Sam introduced them by email in March 2021.")},
	})
	require.NoError(t, err)
	claim := personFactProjectionClaim(personID, target, `"Met at a conference"`, "how-we-met-claim")
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "how-we-met-pinned", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(t, err)
	require.Len(t, result.Decisions, 1)
	assert.Equal(t, personfacts.ReasonPinRetained, result.Decisions[0].Reason)
	assert.Empty(t, result.Projections)
	_, err = st.SetPersonFactPinContext(t.Context(), personID, personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, false, "test")
	require.NoError(t, err)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugHowWeMet})
	require.NoError(t, err)
	require.Len(t, values, 1)
	assert.Equal(t, "Met at a conference", *values[0].Value.Text)
}
