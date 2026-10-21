//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbankdocument "go.kenn.io/docbank/document"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/document"
)

type interruptedDocumentProvider struct {
	cancel context.CancelFunc
	calls  int
}

func (p *interruptedDocumentProvider) EmbedDocuments(context.Context, []vector.DocumentInput) ([][][]float32, error) {
	panic("request boundary required")
}
func (p *interruptedDocumentProvider) EmbedDocumentRequests(ctx context.Context, inputs []vector.DocumentInput, started func(first, count int), completed vector.DocumentRequestFunc) error {
	for i := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if started != nil {
			started(i, 1)
		}
		p.calls++
		if p.cancel != nil {
			p.cancel()
		}
		if err := completed(i, [][][]float32{{{1, 2, 3}}}); err != nil {
			return err
		}
	}
	return nil
}

func interruptedDocumentWorker(st *store.Store, backend document.Backend, provider *interruptedDocumentProvider) *document.Worker {
	return document.NewWorker(document.WorkerDeps{
		Ledger: st, Backend: backend, Provider: provider, Owner: "interrupted-worker", Dimension: 3,
		MaxInputChars: 1000, ContextualDocuments: true, MaxAttempts: 1,
		LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, RetryDelay: time.Minute,
	})
}

func TestDocumentWorkerReturnedResponseSurvivesRunCancellation(t *testing.T) {
	st, generation := seedInterruptedDocuments(t, "first")
	_, backend := openDocumentBackendForTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	provider := &interruptedDocumentProvider{cancel: cancel}
	_, err := interruptedDocumentWorker(st, backend, provider).Run(ctx, document.GenerationID(generation.ID), 1)
	require.ErrorIs(t, err, context.Canceled)
	candidates, err := st.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	var token string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT token FROM document_vector_publications WHERE generation_id = ? AND chunk_id = ?`), generation.ID, candidates[0].ChunkID).Scan(&token))
	saved, err := backend.UnpublishedTokens(t.Context(), document.GenerationID(generation.ID), 3, []string{token})
	require.NoError(t, err)
	assert.True(t, saved[token], "the completed response survives cancellation in the real vector database")
	provider.cancel = nil
	result, err := interruptedDocumentWorker(st, backend, provider).Run(t.Context(), document.GenerationID(generation.ID), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Published, "a rebuilt worker publishes the saved output")
	assert.Equal(t, 1, provider.calls, "a successful request is never repeated")
}

func seedInterruptedDocuments(t *testing.T, chunks ...string) (*store.Store, store.DocumentVectorGeneration) {
	t.Helper()
	requirements := require.New(t)
	normalizationPolicy, err := docbankdocument.NewNormalizePolicy(10000)
	requirements.NoError(err)
	fixture := storetest.New(t)
	profileFingerprint := strings.Repeat("a", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + profileFingerprint, Fingerprint: profileFingerprint,
		Provider: "mistral", Endpoint: "https://api.example.invalid/v1/ocr", Region: "test",
		Model: "ocr-test", RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"application/pdf"}, PolicyJSON: []byte(`{"policy":1}`),
	}
	_, err = fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	requirements.NoError(err)
	requirements.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))
	messageID := fixture.CreateMessage("semantic-search-document")
	hash := strings.Repeat("b", 64)
	attachmentID := addInterruptedDocumentAttachment(t, fixture, messageID, hash)
	occurrence, eligible, err := fixture.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
	requirements.NoError(err)
	requirements.True(eligible)
	claim, err := fixture.Store.ClaimDocumentExtraction(t.Context(), store.DocumentExtractionClaimInput{
		ExtractionID: "semantic-extraction", ProfileID: profile.ID, CanonicalBlobHash: hash,
		ExtractionInputKey: "original", OccurrenceAttachmentID: occurrence.AttachmentID,
		OccurrenceMIMEType: occurrence.MIMEType, OccurrenceMessageType: "email",
		LeaseOwner: "semantic-extractor", LeaseUntil: time.Now().UTC().Add(time.Minute),
		LocalBytes: 128, SourceSequence: occurrence.SourceSequence,
	})
	requirements.NoError(err)
	units := make([]docbankdocument.SourceUnit, len(chunks))
	for i, text := range chunks {
		units[i] = docbankdocument.SourceUnit{Index: i, Markdown: text}
	}
	normalized, err := docbankdocument.NormalizeDocument(docbankdocument.SourceDocument{Family: "pdf", UnitKind: "page", Units: units}, normalizationPolicy)
	requirements.NoError(err)
	requirements.Len(normalized.Chunks, len(chunks))
	publication := store.DocumentExtractionPublication{
		ExtractionID: claim.ExtractionID, ProfileID: claim.ProfileID,
		CanonicalBlobHash: claim.CanonicalBlobHash, ExtractionInputKey: claim.ExtractionInputKey,
		OccurrenceAttachmentID: claim.OccurrenceAttachmentID, OccurrenceMIMEType: claim.OccurrenceMIMEType,
		OccurrenceMessageType: claim.OccurrenceMessageType, LeaseOwner: claim.LeaseOwner, LeaseFence: claim.LeaseFence,
		ReturnedModel: profile.Model, UnitsProcessed: len(units), RequestCount: 1,
		ManifestChecksum: normalized.Checksum, NormalizationVersion: normalized.PolicyVersion,
		DocumentFamily: normalized.Family, UnitKind: normalized.UnitKind,
	}
	for _, unit := range normalized.Units {
		publication.Units = append(publication.Units, store.DocumentPublishedUnit{
			Index: unit.Index, Kind: unit.Kind, Text: unit.Text, Checksum: unit.Checksum, CharCount: unit.CharCount,
		})
	}
	for _, chunk := range normalized.Chunks {
		span := chunk.Spans[0]
		publication.Chunks = append(publication.Chunks, store.DocumentPublishedChunk{
			Key: chunk.Key, Ordinal: chunk.Ordinal, Text: chunk.Text, Checksum: chunk.Checksum, CharCount: chunk.CharCount,
			FirstUnitIndex: span.UnitIndex, LastUnitIndex: span.UnitIndex,
			Spans: []store.DocumentPublishedSpan{{UnitIndex: span.UnitIndex, CharStart: span.CharStart, CharEnd: span.CharEnd}},
		})
	}
	requirements.NoError(fixture.Store.PublishDocumentExtraction(t.Context(), publication))
	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("f", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-test", Dimension: 3,
	})
	requirements.NoError(err)
	return fixture.Store, generation
}
func addInterruptedDocumentAttachment(t *testing.T, fixture *storetest.Fixture, messageID int64, hash string) int64 {
	t.Helper()
	require.NoError(t, fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 128, StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "mime:1",
	}))
	var id int64
	require.NoError(t, fixture.Store.DB().QueryRow(fixture.Store.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&id))
	return id
}
