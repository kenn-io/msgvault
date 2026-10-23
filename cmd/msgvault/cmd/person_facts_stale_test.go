package cmd

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonFactsStaleThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier("email", "alice@example.com", "alice")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	var target personfacts.TargetDescriptor
	for _, item := range catalog.Targets {
		if item.Slug == store.AttributeSlugInterestsFunNow {
			target = item
		}
	}
	require.NotEmpty(target.Key)
	now := time.Now().UTC()
	subject := person.ID
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: person.ID, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "stale", End: "stale-end"}},
		ProgramID: "fixture-program", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64),
		CatalogFingerprint: catalog.Fingerprint, Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: now.Add(-100 * 24 * time.Hour), Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		Claims: []personfacts.ProposedClaim{{
			Target: target, Relation: personfacts.RelationSupport, SubmittedValue: json.RawMessage(`"climbing"`), Origin: personfacts.OriginExtraction,
			Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{PersonID: person.ID, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
				SourceURL: "https://example.com/stale-cli", SubjectPersonID: &subject, SubjectRef: "synthetic-person", Excerpt: "Synthetic evidence", SourceVersion: "source-v1",
				EventTime: now.Add(-200 * 24 * time.Hour), RecordedTime: now.Add(-100 * 24 * time.Hour), IdentityScore: 990,
			}},
		}},
	}, nil)
	require.NoError(err)
	value := "Porto"
	_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: store.AttributeSlugLocation, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}, ActiveFrom: &now, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: httpServer.URL, AllowInsecure: true}})
	id := strconv.FormatInt(person.ID, 10)
	output, err := runPersonFactsCommand(ctx, t, "stale", id)
	require.NoError(err)
	assert.Contains(output, "NEWEST SUPPORT")
	assert.Contains(output, "climbing")
	assert.NotContains(output, "Porto")
	output, err = runPersonFactsCommand(ctx, t, "stale", id, "--all")
	require.NoError(err)
	assert.Contains(output, "climbing")
	assert.Contains(output, "Porto")
	output, err = runPersonFactsCommand(ctx, t, "stale", id, "--json")
	require.NoError(err)
	var rows []struct {
		Stale bool `json:"stale"`
	}
	require.NoError(json.Unmarshal([]byte(output), &rows))
	require.Len(rows, 1)
	assert.True(rows[0].Stale)
}
