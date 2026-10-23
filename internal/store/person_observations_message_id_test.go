package store_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonFactEvidenceMessageIDNewGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "new-message-link", "email", f.aliceID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, messageID, "I prefer chat")
	item := sweepItem(t, loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds, messageID)
	archive := sweepEvidenceInput(t, item)
	public := archive
	public.SourceClass = personfacts.EvidencePublic
	public.SourceURL = "https://example.com/profile"
	public.SourceRef = ""
	public.SpanStart, public.SpanEnd = nil, nil
	public.ContentSHA256 = ""
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	var target personfacts.TargetDescriptor
	for _, candidate := range catalog.Targets {
		if candidate.Slug == store.AttributeSlugPrimaryChannel {
			target = candidate
		}
	}
	require.NotEmpty(target.Key)
	input := personfacts.GenerationInput{
		PersonID: f.alicePersonID, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "new-link", End: "new-link-end"}},
		ProgramID: "message-link-fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: catalog.Fingerprint,
		Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: time.Now().UTC(),
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "policy-v1"},
		Claims: []personfacts.ProposedClaim{{Target: target, Relation: personfacts.RelationSupport, SubmittedValue: []byte(`"chat"`),
			Evidence: []personfacts.EvidenceInput{archive, public}, Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}}},
	}
	result, err := f.store.ApplyPersonFactGenerationContext(t.Context(), input, store.PersonSweepEvidenceAligner{Store: f.store})
	require.NoError(err)
	require.Len(result.Decisions, 1)
	require.Equal(personfacts.DecisionApplied, result.Decisions[0].Action)
	for _, class := range []personfacts.EvidenceSourceClass{personfacts.EvidenceArchive, personfacts.EvidencePublic} {
		var message sql.NullInt64
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT message_id FROM person_fact_evidence WHERE person_id=? AND source_class=?`), f.alicePersonID, class).Scan(&message))
		if class == personfacts.EvidenceArchive {
			assert.Equal(sql.NullInt64{Int64: messageID, Valid: true}, message)
		} else {
			assert.False(message.Valid)
		}
	}
}
