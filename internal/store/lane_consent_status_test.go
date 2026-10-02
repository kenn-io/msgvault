package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestLaneReadinessConsentHistoryPreservesPurposeAndRevocation(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	fixture, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	st := fixture.Store
	read := func(purpose string) bool {
		t.Helper()
		active, err := st.HasAnyActiveLaneConsent(t.Context(), purpose)
		requirements.NoError(err)
		return active
	}
	for _, purpose := range []string{"person_inference", "person_semantic", "document_embedding", "query_embedding"} {
		assertions.False(read(purpose))
	}
	inference := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), inference)
	requirements.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), inference.Fingerprint, "cli")
	requirements.NoError(err)
	assertions.True(read("person_inference"))
	assertions.False(read("person_semantic"), "inference grants cannot authorize semantic embedding")
	active, err := st.HasActivePersonInferenceConsent(t.Context(), strings.Repeat("1", 64))
	requirements.NoError(err)
	assertions.False(active, "historical readiness does not authorize a different current policy")
	_, err = st.RevokePersonInferenceConsent(t.Context(), inference.Fingerprint, "cli")
	requirements.NoError(err)
	assertions.False(read("person_inference"))
	semantic := semanticPersonTestProfile(t)
	_, err = st.EnsurePersonSemanticEmbeddingProfile(t.Context(), semantic)
	requirements.NoError(err)
	_, _, err = st.GrantPersonSemanticEmbeddingConsent(t.Context(), semantic.Fingerprint, "cli")
	requirements.NoError(err)
	assertions.True(read("person_semantic"))
	assertions.False(read("person_inference"))
	_, err = st.RevokePersonSemanticEmbeddingConsent(t.Context(), semantic.Fingerprint, "cli")
	requirements.NoError(err)
	assertions.False(read("person_semantic"))
	_, _, err = st.RecordDocumentVectorConsent(t.Context(), store.DocumentVectorConsentSpec{DocumentVectorGenerationSpec: generation.DocumentVectorGenerationSpec, EgressFingerprint: strings.Repeat("d", 64), Purpose: "document_embedding"}, time.Now())
	requirements.NoError(err)
	assertions.True(read("document_embedding"))
	assertions.False(read("query_embedding"), "upload consent does not authorize queries")
	_, err = st.HasAnyActiveLaneConsent(t.Context(), "arbitrary_table")
	requirements.Error(err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.HasAnyActiveLaneConsent(canceled, "document_embedding")
	requirements.ErrorIs(err, context.Canceled)
}
