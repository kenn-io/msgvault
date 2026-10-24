package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

func observationBatchFixture(t *testing.T, text string) (personSweepJournalFixture, personobs.BatchInput, int64) {
	t.Helper()
	f := newPersonSweepJournalFixture(t, true, false)
	message := f.insertMessage(t, "observation", "email", f.aliceID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, message, text)
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, message)
	input := sweepEvidenceInput(t, item)
	quote := text
	offset := strings.Index(input.Excerpt, quote)
	require.NotEqual(t, -1, offset)
	fingerprint := strings.Repeat("a", 64)
	generation := insertPersonFactSchemaGeneration(t, f.store, f.alicePersonID, "observation-generation")
	return f, personobs.BatchInput{PersonID: f.alicePersonID, ProgramFingerprint: fingerprint, Actor: "owner", Generation: &personfacts.GenerationInput{PersonID: f.alicePersonID, ProgramFingerprint: fingerprint}, Proposals: []personobs.ProposedObservation{{PersonID: f.alicePersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginExtraction, Kind: personobs.KindState, Content: "[PERSON] runs", Attribution: personobs.AttributionSelf, ConfidenceBP: 800, Support: []personobs.SupportInput{{Evidence: input, Quote: quote, QuoteStart: utf8.RuneCountInString(input.Excerpt[:offset])}}, Stability: personobs.StabilityDurable}}}, generation
}
func TestPrepareObservationRejectsFabricatedSpan(t *testing.T) {
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	in.Proposals[0].Support[0].Evidence.Excerpt = strings.Replace(in.Proposals[0].Support[0].Evidence.Excerpt, "Synthetic", "Inventedx", 1)
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Len(batch.Items, 1)
	require.NotNil(batch.Items[0].Failure)
	assert.Equal(t, "unaligned-evidence", batch.Items[0].Failure.Reason)
}
func TestPrepareObservationRejectsWrongPersonGeneration(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	in.Generation.PersonID = f.bobPersonID
	_, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.Error(t, err)
	in.Generation.PersonID = f.alicePersonID
	in.Generation.ProgramFingerprint = strings.Repeat("b", 64)
	_, err = f.store.PreparePersonObservationBatch(t.Context(), in)
	require.Error(t, err)
}
func TestPrepareObservationRejectsContextCitations(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	in.Proposals[0].Attribution = personobs.AttributionAssent
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	require.NotNil(t, batch.Items[0].Failure)
	assert.Equal(t, "context-citations-unavailable", batch.Items[0].Failure.Reason)
}
func TestPrepareObservationQuoteSubspanNonzeroOffset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "界 I run and I run")
	support := &in.Proposals[0].Support[0]
	offset := strings.LastIndex(support.Evidence.Excerpt, "I run")
	support.Quote = "I run"
	support.QuoteStart = utf8.RuneCountInString(support.Evidence.Excerpt[:offset])
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	require.Len(batch.Items[0].Citations, 1)
	citation := batch.Items[0].Citations[0]
	digest := sha256.Sum256([]byte("I run"))
	assert.Equal(hex.EncodeToString(digest[:]), citation.QuoteSHA256)
	assert.Equal(support.QuoteStart, citation.QuoteStart)
	assert.Equal(support.QuoteStart+5, citation.QuoteEnd)
	assert.True(citation.Leaf.AuthoredByPerson)
	assert.Positive(citation.Leaf.MessageID)
	assert.Equal(f.conversationID, citation.Leaf.ConversationID)
}
func TestPrepareObservationCategoryFailClosed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I receive chemotherapy")
	in.Proposals[0].Content = "[PERSON] receives chemotherapy"
	in.Proposals[0].ModelCategories = []personobs.Category{personobs.CategoryPolitics}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	assert.ElementsMatch([]personobs.Category{personobs.CategoryHealth, personobs.CategoryPolitics}, batch.Items[0].Categories)
}

func TestPrepareObservationPreservesExplicitSensitivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	in.Proposals[0].Sensitive = true

	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	assert.Empty(batch.Items[0].Categories)
	assert.True(batch.Items[0].Proposal.Sensitive)
}

func TestPrepareObservationNormalizesValidityBoundsToUTC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	west := time.FixedZone("west", -7*60*60)
	east := time.FixedZone("east", 9*60*60)
	from := time.Date(2026, 10, 1, 0, 0, 0, 123456789, west)
	until := from.Add(30 * 24 * time.Hour).In(east)
	in.Proposals[0].ValidFrom = &from
	in.Proposals[0].ValidUntil = &until

	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	require.NotNil(batch.Items[0].Proposal.ValidFrom)
	require.NotNil(batch.Items[0].Proposal.ValidUntil)
	assert.Equal(from.UTC().Truncate(time.Microsecond), *batch.Items[0].Proposal.ValidFrom)
	assert.Equal(until.UTC().Truncate(time.Microsecond), *batch.Items[0].Proposal.ValidUntil)
}
func TestPrepareObservationRecordsExclusionReason(t *testing.T) {
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET list_id='list.example.test' WHERE id=?`), ref.MessageID)
	require.NoError(err)
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	assert.Equal(t, personobs.ExclusionReasonList, batch.Items[0].Citations[0].Exclusion)
}
func TestPrepareObservationStabilityClamp(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := from.AddDate(0, 0, 30)
	in.Proposals[0].ValidFrom = &from
	in.Proposals[0].ValidUntil = &until
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	require.Nil(t, batch.Items[0].Failure)
	assert.Equal(t, personobs.StabilityVolatile, batch.Items[0].Stability)
}

func TestPrepareObservationRecordsDraftExclusion(t *testing.T) {
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(in.Proposals[0].Support[0].Evidence.SourceRef)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET draft_authored=TRUE WHERE id=?`), ref.MessageID)
	require.NoError(err)
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)
	assert.Equal(t, personobs.ExclusionReasonDraft, batch.Items[0].Citations[0].Exclusion)
}
