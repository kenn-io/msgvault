package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

var personBriefAPINow = time.Date(2026, time.August, 29, 18, 42, 10, 0, time.UTC)

// personBriefAPIExcerpt is the verbatim archive text the brief's evidence
// carries. No brief response may ever contain it.
const personBriefAPIExcerpt = "Synthetic archive payload the brief must never echo"

type personBriefAPIFixture struct {
	server   *Server
	store    *store.Store
	personID int64
	evidence []personfacts.Evidence
}

// newPersonBriefAPIFixture builds a tracked, enrolled person with real
// person_fact_generations and person_fact_evidence rows written through the
// exported ledger path. Brief versions are seeded with SQL because the store's
// brief writer is reachable only from inside the sweep's apply transaction;
// every read the handlers make is the production store read.
func newPersonBriefAPIFixture(t *testing.T) *personBriefAPIFixture {
	t.Helper()
	srv, wrapped := newIdentityLinkTestServer(t)
	participantID := wrapped.mustParticipant(t, "brief@example.test", "brief", "example.test")
	person, _, err := wrapped.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	st := wrapped.Store
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	_, err = st.SetPersonBriefEnrollmentContext(t.Context(), person.ID, true, "test-owner", false)
	require.NoError(t, err)

	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	var target personfacts.TargetDescriptor
	for _, candidate := range catalog.Targets {
		if candidate.Slug == store.AttributeSlugPrimaryChannel {
			target = candidate
			break
		}
	}
	require.NotEmpty(t, target.Key, "fixture needs one catalog target")
	subject := person.ID
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: person.ID,
		SourceCursors: []personfacts.SourceCursor{{
			Lane: "brief-fixture", Start: "start", End: "end",
		}},
		ProgramID: "fixture-program", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("b", 64), CatalogFingerprint: "fixture-catalog",
		Provider: "fixture-provider", ProviderVersion: "v1",
		Model: "fixture-model", ModelVersion: "v1", ResolvedAt: personBriefAPINow,
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		Claims: []personfacts.ProposedClaim{{
			Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(`"chat"`), Origin: personfacts.OriginBrief,
			Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				PersonID: person.ID, SourceClass: personfacts.EvidencePublic,
				Directness: personfacts.DirectOther, Authority: personfacts.AuthorityAuthoritative,
				SourceRef: "message:1", SourceURL: "https://example.test/brief",
				SubjectPersonID: &subject, SubjectRef: "synthetic-person",
				Excerpt: personBriefAPIExcerpt, SourceVersion: "source-v1",
				EventTime: personBriefAPINow.Add(-time.Hour), RecordedTime: personBriefAPINow,
				IdentityScore: 990,
			}},
		}},
	}, nil)
	require.NoError(t, err)

	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), person.ID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, evidence)
	return &personBriefAPIFixture{
		server: srv, store: st, personID: person.ID, evidence: evidence,
	}
}

func (f *personBriefAPIFixture) generationID(t *testing.T) int64 {
	t.Helper()
	var id int64
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT id FROM person_fact_generations WHERE person_id = ? ORDER BY id LIMIT 1`),
		f.personID).Scan(&id))
	return id
}

// personBriefAPIJSONBind matches the JSON bind the store's own writer uses, so
// the fixture inserts JSON rather than bytes on PostgreSQL.
func personBriefAPIJSONBind() string {
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(testDB, "postgres://") || strings.HasPrefix(testDB, "postgresql://") {
		return "?::jsonb"
	}
	return "?"
}

type personBriefAPISeed struct {
	structured  string
	rendered    string
	boundary    string
	dropped     int
	generatedAt time.Time
	evidenceIDs []int64
}

// seedBrief inserts one version as the current one, superseding whatever was
// current before, exactly as the sweep's apply transaction does.
func (f *personBriefAPIFixture) seedBrief(t *testing.T, seed personBriefAPISeed) {
	t.Helper()
	ctx := t.Context()
	_, err := f.store.DB().ExecContext(ctx, f.store.Rebind(
		`UPDATE person_briefs SET status = ?, superseded_at = ?
		 WHERE person_id = ? AND status = ?`),
		store.PersonBriefStatusSuperseded, seed.generatedAt.UTC(), f.personID,
		store.PersonBriefStatusCurrent)
	require.NoError(t, err)
	var version int
	require.NoError(t, f.store.DB().QueryRowContext(ctx, f.store.Rebind(
		`SELECT COALESCE(MAX(version), 0) + 1 FROM person_briefs WHERE person_id = ?`),
		f.personID).Scan(&version))
	bind := personBriefAPIJSONBind()
	var briefID int64
	require.NoError(t, f.store.DB().QueryRowContext(ctx, f.store.Rebind(`
		INSERT INTO person_briefs
			(person_id, version, generation_id, status, program_id, program_version,
			 program_fingerprint, provider, provider_version, model, model_version,
			 provider_policy_fingerprint, boundary_json, structured_json, rendered_text,
			 renderer_policy, dropped_item_count, generated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+bind+`, `+bind+`, ?, ?, ?, ?)
		RETURNING id`),
		f.personID, version, f.generationID(t), store.PersonBriefStatusCurrent,
		peoplesweep.BriefProgramID, peoplesweep.BriefProgramVersion,
		peoplesweep.BriefProgramFingerprint(), "openai_chat", "provider-v1",
		"gpt-test", "model-v1", "policy-fingerprint", seed.boundary, seed.structured,
		seed.rendered, peoplesweep.BriefRendererPolicyV1, seed.dropped,
		seed.generatedAt.UTC()).Scan(&briefID))
	for ordinal, evidenceID := range seed.evidenceIDs {
		_, err := f.store.DB().ExecContext(ctx, f.store.Rebind(
			`INSERT INTO person_brief_evidence (brief_id, evidence_id, ordinal)
			 VALUES (?, ?, ?)`), briefID, evidenceID, ordinal)
		require.NoError(t, err)
	}
}

// personBriefAPIStructure is a validated brief structure over the fixture's one
// evidence item, rendered by the real renderer so the seeded rendered_text and
// structured_json agree exactly as the worker stores them.
// personBriefAPIEvidenceID is the packet evidence ID the seeded structure
// cites. Its exact value never reaches the API, which reads the durable
// pointers instead, so one synthetic ID covers every case here.
const personBriefAPIEvidenceID = "evidence-1"

func personBriefAPIStructure() peoplesweep.BriefOutput {
	evidenceID := personBriefAPIEvidenceID
	return peoplesweep.BriefOutput{
		LastMeaningfulInteraction: &peoplesweep.BriefInteraction{
			EvidenceID: evidenceID, Summary: "they were preparing for a role change",
		},
		Highlights: []peoplesweep.BriefHighlight{{
			Text: "they were preparing for a role change", Speaker: peoplesweep.BriefSpeakerPerson,
			EvidenceIDs: []string{evidenceID}, ConfidenceBasisPoints: 800,
		}},
		FollowUps: []peoplesweep.BriefFollowUp{{
			Question: "how the transition went", Why: "the role change was pending",
			HighlightIndex: 0, EvidenceIDs: []string{evidenceID},
		}},
		Appreciations: []peoplesweep.BriefAppreciation{},
		Uncertainties: []peoplesweep.BriefUncertainty{{
			Text: "the move may already have happened", Kind: peoplesweep.BriefUncertaintyStale,
			EvidenceIDs: []string{evidenceID},
		}},
		PossibleAttributes: []peoplesweep.ExtractedClaim{},
	}
}

// personBriefAPISeedFor renders the structure the way the worker does, with a
// header carrying the deterministic contact date and channel the API cannot
// recompute.
func personBriefAPISeedFor(
	t *testing.T, output peoplesweep.BriefOutput, generatedAt time.Time, evidenceIDs []int64,
) personBriefAPISeed {
	t.Helper()
	structured, err := json.Marshal(output)
	require.NoError(t, err)
	sentences := []string{}
	if output.LastMeaningfulInteraction != nil {
		sentences = append(sentences,
			"Last time you talked (Aug 29, chat): "+output.LastMeaningfulInteraction.Summary+".")
	}
	for _, highlight := range output.Highlights {
		sentences = append(sentences, "They said "+highlight.Text+".")
	}
	for _, followUp := range output.FollowUps {
		sentences = append(sentences, "You may want to ask "+followUp.Question+".")
	}
	for _, appreciation := range output.Appreciations {
		sentences = append(sentences, "You said you appreciated "+appreciation.Text+".")
	}
	for _, uncertainty := range output.Uncertainties {
		sentences = append(sentences, "Check before assuming: "+uncertainty.Text+".")
	}
	boundary, err := json.Marshal(peoplesweep.BriefBoundary{
		Lanes: []string{"conversation_text"}, FromEventTime: generatedAt.Add(-48 * time.Hour),
		ThroughEventTime: generatedAt, ThroughSequence: 184233, ItemCount: 1,
		InputBytes: 512, PacketSHA256: strings.Repeat("c", 64),
	})
	require.NoError(t, err)
	return personBriefAPISeed{
		structured: string(structured), rendered: strings.Join(sentences, " "),
		boundary: string(boundary), generatedAt: generatedAt, evidenceIDs: evidenceIDs,
	}
}

func personBriefPath(personID int64, suffix string) string {
	return fmt.Sprintf("/api/v1/people/%d/brief%s", personID, suffix)
}

// TestPersonBriefHTTPStripsControlCharactersFromBriefProse pins the boundary
// sanitization. The paragraph and the structure are provider prose derived
// from other people's messages, so a terminal escape a sender got into them
// must not reach a client through the API; the route strips control
// characters the way the TUI does, while the identifiers stay as stored.
func TestPersonBriefHTTPStripsControlCharactersFromBriefProse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefAPIFixture(t)
	output := personBriefAPIStructure()
	// Use a non-NUL C0 control so PostgreSQL can persist the raw fixture.
	output.LastMeaningfulInteraction.Summary = "they were\x1b[2J preparing\x01 for a role change"
	output.Highlights[0].Text = "they\x1b]0;owned\x07 mentioned a move"
	output.FollowUps[0].Why = "the\u009b role change was pending"
	output.Uncertainties[0].Text = "the move\x08 may already have happened"
	seed := personBriefAPISeedFor(t, output, personBriefAPINow, []int64{f.evidence[0].ID})
	requirements.Contains(seed.rendered, "\x1b", "the fixture stores the raw escape")
	f.seedBrief(t, seed)

	response := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, ""), nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	body := response.Body.String()
	for _, control := range []string{"\\u001b", "\\u0001", "\\u0007", "\\u0008", "\\u009b"} {
		assertions.NotContains(body, control, "control sequence reached the client")
	}

	var brief PersonBrief
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &brief))
	assertions.Contains(brief.RenderedText, "they were preparing for a role change")
	requirements.Len(brief.Sentences, 4,
		"the sentence map is rebuilt from the stored values before sanitization")
	assertions.Contains(brief.Sentences[1].Text, "they mentioned a move")
	var structured peoplesweep.BriefOutput
	requirements.NoError(json.Unmarshal(brief.Structured, &structured))
	assertions.Equal("they were preparing for a role change", structured.LastMeaningfulInteraction.Summary)
	assertions.Equal("they mentioned a move", structured.Highlights[0].Text)
	assertions.Equal("the role change was pending", structured.FollowUps[0].Why)
	assertions.Equal("the move may already have happened", structured.Uncertainties[0].Text)
	assertions.Equal(personBriefAPIEvidenceID, structured.LastMeaningfulInteraction.EvidenceID,
		"identifiers are left as stored")
	assertions.Equal([]string{personBriefAPIEvidenceID}, structured.Highlights[0].EvidenceIDs)
}

func TestPersonBriefHTTPReturnsCurrentVersionWithSentencesAndEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefAPIFixture(t)
	output := personBriefAPIStructure()
	seed := personBriefAPISeedFor(t, output, personBriefAPINow, []int64{f.evidence[0].ID})
	seed.dropped = 2
	f.seedBrief(t, seed)

	response := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, ""), nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))

	var brief PersonBrief
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &brief))
	assertions.Equal(1, brief.Version)
	assertions.Equal(store.PersonBriefStatusCurrent, brief.Status)
	assertions.Equal(personBriefAPINow, brief.GeneratedAt.UTC())
	assertions.Equal(seed.rendered, brief.RenderedText)
	assertions.Equal(2, brief.DroppedItemCount)
	assertions.Equal(peoplesweep.BriefProgramID, brief.ProgramID)
	assertions.Equal(peoplesweep.BriefProgramVersion, brief.ProgramVersion)
	assertions.Equal("openai_chat", brief.Provider)
	assertions.Equal("gpt-test", brief.Model)
	assertions.Nil(brief.RejectedAt)
	assertions.Empty(brief.RejectedReason)
	assertions.Nil(brief.SupersededAt)

	// Every rendered sentence maps to one structured item and the sentences
	// rejoin exactly into the stored paragraph.
	requirements.Len(brief.Sentences, 4)
	assertions.Equal(string(peoplesweep.BriefSentenceLastInteraction), brief.Sentences[0].Kind)
	assertions.Equal(string(peoplesweep.BriefSentenceHighlight), brief.Sentences[1].Kind)
	assertions.Equal(string(peoplesweep.BriefSentenceFollowUp), brief.Sentences[2].Kind)
	assertions.Equal(string(peoplesweep.BriefSentenceUncertainty), brief.Sentences[3].Kind)
	texts := make([]string, 0, len(brief.Sentences))
	for _, sentence := range brief.Sentences {
		texts = append(texts, sentence.Text)
	}
	assertions.Equal(brief.RenderedText, strings.Join(texts, " "))

	// The join key: every item here cites the fixture's one evidence item, so
	// every sentence names ordinal 0 of the response's own evidence list.
	assertions.Equal(peoplesweep.BriefRendererPolicyV1, brief.RendererPolicy)
	for _, sentence := range brief.Sentences {
		assertions.Equal([]int{0}, sentence.EvidenceOrdinals, "sentence %s", sentence.Kind)
	}

	var structured peoplesweep.BriefOutput
	requirements.NoError(json.Unmarshal(brief.Structured, &structured))
	assertions.Equal(output.Highlights, structured.Highlights)
	var boundary peoplesweep.BriefBoundary
	requirements.NoError(json.Unmarshal(brief.Boundary, &boundary))
	assertions.Equal(int64(184233), boundary.ThroughSequence)

	requirements.Len(brief.Evidence, 1)
	assertions.Equal(0, brief.Evidence[0].Ordinal)
	assertions.Equal(f.evidence[0].ID, brief.Evidence[0].EvidenceID)
	assertions.Equal("message:1", brief.Evidence[0].SourceRef)
	assertions.True(brief.Evidence[0].Supported)

	assertions.NotContains(response.Body.String(), personBriefAPIExcerpt,
		"the brief response must never carry an archive excerpt")
}

// TestPersonBriefHTTPOmitsSentenceOrdinalsWhenTheCitationOrderCannotBeRebuilt
// covers the fail-closed half of the join key: the stored structure cites two
// distinct packet items but only one pointer row survived alignment, so the
// walk cannot say which pointer belongs to which sentence and every sentence
// reports no ordinals rather than a wrong one.
func TestPersonBriefHTTPOmitsSentenceOrdinalsWhenTheCitationOrderCannotBeRebuilt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefAPIFixture(t)
	output := personBriefAPIStructure()
	output.Uncertainties[0].EvidenceIDs = []string{"evidence-2"}
	f.seedBrief(t, personBriefAPISeedFor(t, output, personBriefAPINow, []int64{f.evidence[0].ID}))

	response := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, ""), nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())

	var brief PersonBrief
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &brief))
	assertions.Equal(peoplesweep.BriefRendererPolicyV1, brief.RendererPolicy)
	requirements.Len(brief.Evidence, 1, "the whole-brief citation list is still served")
	requirements.Len(brief.Sentences, 4, "the paragraph the owner read is still mapped")
	for _, sentence := range brief.Sentences {
		assertions.Empty(sentence.EvidenceOrdinals, "sentence %s", sentence.Kind)
	}
}

func TestPersonBriefHTTPWithoutLastInteraction(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("mismatch=%t", mismatch), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newPersonBriefAPIFixture(t)
			output := personBriefAPIStructure()
			output.LastMeaningfulInteraction = nil
			seed := personBriefAPISeedFor(t, output, personBriefAPINow, []int64{f.evidence[0].ID})
			if mismatch {
				seed.rendered = "Unrelated text. " + seed.rendered
			}
			f.seedBrief(t, seed)
			response := doRequest(t, f.server.Router(), http.MethodGet,
				personBriefPath(f.personID, ""), nil, nil)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var brief PersonBrief
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &brief))
			assertions.Equal(seed.rendered, brief.RenderedText)
			if mismatch {
				assertions.Empty(brief.Sentences, "a mismatched paragraph must not be assigned to a highlight")
				return
			}
			requirements.Len(brief.Sentences, 3)
			assertions.Equal(PersonBriefSentence{
				Kind: "highlight", Index: 0,
				Text: "They said they were preparing for a role change.", EvidenceOrdinals: []int{0},
			}, brief.Sentences[0])
			assertions.Equal("follow_up", brief.Sentences[1].Kind)
			assertions.Equal("uncertainty", brief.Sentences[2].Kind)
		})
	}
}

func TestPersonBriefHTTPReportsMissingCurrentVersion(t *testing.T) {
	assertions := assert.New(t)
	f := newPersonBriefAPIFixture(t)

	response := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, ""), nil, nil)
	assertions.Equal(http.StatusNotFound, response.Code)
	assertions.Contains(response.Body.String(), "person_brief_not_found")

	invalid := doRequest(t, f.server.Router(), http.MethodGet,
		"/api/v1/people/0/brief", nil, nil)
	assertions.Equal(http.StatusBadRequest, invalid.Code)
	assertions.Contains(invalid.Body.String(), "invalid_person_id")
}

func TestPersonBriefHTTPListsVersionsNewestFirst(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefAPIFixture(t)
	first := personBriefAPISeedFor(t, personBriefAPIStructure(),
		personBriefAPINow.Add(-72*time.Hour), []int64{f.evidence[0].ID})
	f.seedBrief(t, first)
	second := personBriefAPISeedFor(t, personBriefAPIStructure(),
		personBriefAPINow, []int64{f.evidence[0].ID})
	f.seedBrief(t, second)

	response := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, "/versions"), nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var versions PersonBriefVersionsResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &versions))
	requirements.Len(versions.Versions, 2)
	assertions.Equal(2, versions.Versions[0].Version)
	assertions.Equal(store.PersonBriefStatusCurrent, versions.Versions[0].Status)
	assertions.Equal(1, versions.Versions[1].Version)
	assertions.Equal(store.PersonBriefStatusSuperseded, versions.Versions[1].Status)
	assertions.NotNil(versions.Versions[1].SupersededAt)
	assertions.NotEmpty(versions.Versions[1].Sentences)
	assertions.NotContains(response.Body.String(), personBriefAPIExcerpt)

	bounded := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, "/versions?limit=1"), nil, nil)
	requirements.Equal(http.StatusOK, bounded.Code, bounded.Body.String())
	requirements.NoError(json.Unmarshal(bounded.Body.Bytes(), &versions))
	requirements.Len(versions.Versions, 1)
	assertions.Equal(2, versions.Versions[0].Version)

	rejected := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, "/versions?limit=0"), nil, nil)
	assertions.Equal(http.StatusBadRequest, rejected.Code)
}

func TestPersonBriefHTTPRejectsTheCurrentVersionOnce(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefAPIFixture(t)
	f.seedBrief(t, personBriefAPISeedFor(t, personBriefAPIStructure(),
		personBriefAPINow, []int64{f.evidence[0].ID}))

	response := doRequest(t, f.server.Router(), http.MethodPost,
		personBriefPath(f.personID, "/reject"),
		[]byte(`{"reason":"it described the wrong thread"}`), nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var brief PersonBrief
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &brief))
	assertions.Equal(store.PersonBriefStatusRejected, brief.Status)
	assertions.Equal("it described the wrong thread", brief.RejectedReason)
	requirements.NotNil(brief.RejectedAt)
	assertions.NotEmpty(brief.RenderedText, "a rejected version stays readable")

	// The rejected version leaves no current version behind.
	missing := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, ""), nil, nil)
	assertions.Equal(http.StatusNotFound, missing.Code)

	repeat := doRequest(t, f.server.Router(), http.MethodPost,
		personBriefPath(f.personID, "/reject"), []byte(`{"reason":"again"}`), nil)
	assertions.Equal(http.StatusNotFound, repeat.Code)
	assertions.Contains(repeat.Body.String(), "person_brief_not_found")

	malformed := doRequest(t, f.server.Router(), http.MethodPost,
		personBriefPath(f.personID, "/reject"), []byte(`{"unknown":"field"}`), nil)
	assertions.Equal(http.StatusBadRequest, malformed.Code)
}

func TestPersonBriefEvidenceReportsUnsupportedAfterAStatusEvent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefAPIFixture(t)
	f.seedBrief(t, personBriefAPISeedFor(t, personBriefAPIStructure(),
		personBriefAPINow, []int64{f.evidence[0].ID}))
	_, err := f.store.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: f.personID,
		SourceCursors: []personfacts.SourceCursor{{
			Lane: "brief-fixture-status", Start: "s", End: "s-end",
		}},
		ProgramID: "fixture-program", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("b", 64), CatalogFingerprint: "fixture-catalog",
		Provider: "fixture-provider", ProviderVersion: "v1",
		ResolvedAt: personBriefAPINow.Add(time.Hour),
		Policy:     personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		EvidenceStatusChanges: []personfacts.EvidenceStatusChange{{
			EvidenceKey: f.evidence[0].Key, SourceVersion: "source-v1",
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}},
	}, nil)
	requirements.NoError(err)

	response := doRequest(t, f.server.Router(), http.MethodGet,
		personBriefPath(f.personID, ""), nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var brief PersonBrief
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &brief))
	requirements.Len(brief.Evidence, 1)
	assertions.False(brief.Evidence[0].Supported,
		"an invalidated source is reported, not hidden")
}

func TestPersonBriefEnrollmentHTTPGetsAndReplacesState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	participantID := wrapped.mustParticipant(t, "enroll@example.test", "enroll", "example.test")
	person, _, err := wrapped.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	path := fmt.Sprintf("/api/v1/people/%d/brief-enrollment", person.ID)

	response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var state store.PersonBriefEnrollment
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &state))
	assertions.Equal(person.ID, state.PersonID)
	assertions.False(state.Enrolled)
	assertions.Nil(state.EnabledAt)

	untracked := doRequest(t, srv.Router(), http.MethodPut, path,
		[]byte(`{"enrolled":true}`), nil)
	assertions.Equal(http.StatusConflict, untracked.Code)
	assertions.Contains(untracked.Body.String(), "person_brief_not_tracked")
	assertions.Contains(untracked.Body.String(), "msgvault person track")

	tracked := doRequest(t, srv.Router(), http.MethodPut, path,
		[]byte(`{"enrolled":true,"track":true}`), nil)
	requirements.Equal(http.StatusOK, tracked.Code, tracked.Body.String())
	requirements.NoError(json.Unmarshal(tracked.Body.Bytes(), &state))
	assertions.True(state.Enrolled)
	requirements.NotNil(state.EnabledAt)
	assertions.Equal("api", state.Actor)

	trackingState, err := wrapped.GetPersonTrackingContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.True(trackingState.Tracked, "--track enrolls and tracks in one step")

	unenrolled := doRequest(t, srv.Router(), http.MethodPut, path,
		[]byte(`{"enrolled":false}`), nil)
	requirements.Equal(http.StatusOK, unenrolled.Code, unenrolled.Body.String())
	requirements.NoError(json.Unmarshal(unenrolled.Body.Bytes(), &state))
	assertions.False(state.Enrolled)
	assertions.Nil(state.EnabledAt)
}

func TestPersonBriefEnrollmentHTTPValidatesRequests(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	participantID := wrapped.mustParticipant(t, "enroll2@example.test", "enroll2", "example.test")
	person, _, err := wrapped.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	path := fmt.Sprintf("/api/v1/people/%d/brief-enrollment", person.ID)

	missing := doRequest(t, srv.Router(), http.MethodPut, path, []byte(`{}`), nil)
	assertions.Equal(http.StatusBadRequest, missing.Code)
	assertions.Contains(missing.Body.String(), "enrolled is required")

	null := doRequest(t, srv.Router(), http.MethodPut, path,
		[]byte(`{"enrolled":null}`), nil)
	assertions.Equal(http.StatusBadRequest, null.Code)

	absent := doRequest(t, srv.Router(), http.MethodGet,
		"/api/v1/people/999999/brief-enrollment", nil, nil)
	assertions.Equal(http.StatusNotFound, absent.Code)
	assertions.Contains(absent.Body.String(), "person_profile_not_found")
}

func TestPersonBriefGenerateHTTPRunsTheDaemonWorker(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonBriefAPIFixture(t)
	path := personBriefPath(f.personID, "/generate")

	unavailable := doRequest(t, f.server.Router(), http.MethodPost, path, nil, nil)
	assertions.Equal(http.StatusServiceUnavailable, unavailable.Code)
	assertions.Contains(unavailable.Body.String(), "brief_generation_unavailable")

	var requested int64
	f.server.SetPersonBriefGenerator(
		func(_ context.Context, personID int64) (PersonBriefRun, error) {
			requested = personID
			return PersonBriefRun{
				RunID: "run-1", AttemptID: "attempt-1", BriefVersion: 3,
			}, nil
		})
	response := doRequest(t, f.server.Router(), http.MethodPost, path, nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal(f.personID, requested)
	var run PersonBriefRun
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &run))
	assertions.Equal("run-1", run.RunID)
	assertions.Equal("attempt-1", run.AttemptID)
	assertions.Equal(3, run.BriefVersion)
	assertions.Empty(run.BriefFailureClass)

	f.server.SetPersonBriefGenerator(
		func(context.Context, int64) (PersonBriefRun, error) {
			return PersonBriefRun{RunID: "run-2", AttemptID: "attempt-2",
				BriefFailureClass: string(peoplesweep.FailureBudget)}, nil
		})
	deferred := doRequest(t, f.server.Router(), http.MethodPost, path, nil, nil)
	requirements.Equal(http.StatusOK, deferred.Code, deferred.Body.String())
	requirements.NoError(json.Unmarshal(deferred.Body.Bytes(), &run))
	assertions.Zero(run.BriefVersion)
	assertions.Equal(string(peoplesweep.FailureBudget), run.BriefFailureClass)
}

func TestPersonBriefGenerateHTTPRefusesAnUnenrolledPerson(t *testing.T) {
	assertions := assert.New(t)
	f := newPersonBriefAPIFixture(t)
	f.server.SetPersonBriefGenerator(
		func(context.Context, int64) (PersonBriefRun, error) {
			return PersonBriefRun{}, fmt.Errorf("person %d: %w", f.personID,
				peoplesweep.ErrPersonBriefNotEnrolled)
		})

	response := doRequest(t, f.server.Router(), http.MethodPost,
		personBriefPath(f.personID, "/generate"), nil, nil)
	assertions.Equal(http.StatusConflict, response.Code)
	assertions.Contains(response.Body.String(), "person_brief_not_enrolled")

	f.server.SetPersonBriefGenerator(
		func(context.Context, int64) (PersonBriefRun, error) {
			return PersonBriefRun{}, errors.New("provider exploded")
		})
	failed := doRequest(t, f.server.Router(), http.MethodPost,
		personBriefPath(f.personID, "/generate"), nil, nil)
	assertions.Equal(http.StatusInternalServerError, failed.Code)
	assertions.NotContains(failed.Body.String(), "provider exploded",
		"internal failures are logged, not returned")
}

// TestPersonBriefGenerateHTTPRefusesADisabledLaneAndARefusingProfile pins the
// codes for the gates a forced generation used to hit silently: a 200 carrying
// brief_version 0 and an empty failure class read as "nothing happened, and no
// reason", and a profile without a brief lane used to fail inside the attempt.
func TestPersonBriefGenerateHTTPRefusesADisabledLaneAndARefusingProfile(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "lane disabled", err: peoplesweep.ErrPersonBriefLaneDisabled,
			code: "person_brief_lane_disabled"},
		{name: "policy refuses the packet", err: peoplesweep.ErrPersonBriefPolicyRefused,
			code: "person_brief_policy_refused"},
		{name: "profile has no brief lane", err: peoplesweep.ErrPersonBriefNoSupportedLane,
			code: "person_brief_no_supported_lane"},
		{name: "another worker holds the lease", err: peoplesweep.ErrPersonBriefBusy,
			code: "person_brief_busy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			f := newPersonBriefAPIFixture(t)
			f.server.SetPersonBriefGenerator(
				func(context.Context, int64) (PersonBriefRun, error) {
					return PersonBriefRun{}, fmt.Errorf("person %d: %w", f.personID, test.err)
				})

			response := doRequest(t, f.server.Router(), http.MethodPost,
				personBriefPath(f.personID, "/generate"), nil, nil)
			assertions.Equal(http.StatusConflict, response.Code, response.Body.String())
			assertions.Contains(response.Body.String(), test.code)
			assertions.NotContains(response.Body.String(), `"brief_version"`,
				"a refusal is not a run result")
		})
	}
}

func TestPersonBriefGenerateIsNotBoundedByTheStandardRequestTimeout(t *testing.T) {
	assert := assert.New(t)
	assert.True(isLongDaemonRequest("/api/v1/people/42/brief/generate"),
		"a manual brief runs a provider call and must not be cut off at the request timeout")
	assert.False(isLongDaemonRequest("/api/v1/people/42/brief"))
	assert.False(isLongDaemonRequest("/api/v1/people/abc/brief/generate"))
	// The prefix and the suffix are the same length, so this path matches both
	// while being shorter than the two together. It reaches the predicate from
	// any unauthenticated request, ahead of routing and auth.
	assert.False(isLongDaemonRequest("/api/v1/people/brief/generate"))
	assert.False(isLongDaemonRequest("/brief/generate"))
	assert.False(isLongDaemonRequest("/api/v1/people/"))
	assert.False(isLongDaemonRequest(""))
}

func TestPersonBriefOpenAPIContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	document := OpenAPIDocument()

	brief := document.Paths["/api/v1/people/{id}/brief"]
	requirements.NotNil(brief)
	requirements.NotNil(brief.Get)
	assertions.Equal("getPersonBrief", brief.Get.OperationID)

	versions := document.Paths["/api/v1/people/{id}/brief/versions"]
	requirements.NotNil(versions)
	requirements.NotNil(versions.Get)
	assertions.Equal("listPersonBriefVersions", versions.Get.OperationID)

	reject := document.Paths["/api/v1/people/{id}/brief/reject"]
	requirements.NotNil(reject)
	requirements.NotNil(reject.Post)
	assertions.Equal("rejectPersonBrief", reject.Post.OperationID)

	generate := document.Paths["/api/v1/people/{id}/brief/generate"]
	requirements.NotNil(generate)
	requirements.NotNil(generate.Post)
	assertions.Equal("generatePersonBrief", generate.Post.OperationID)

	enrollment := document.Paths["/api/v1/people/{id}/brief-enrollment"]
	requirements.NotNil(enrollment)
	requirements.NotNil(enrollment.Get)
	requirements.NotNil(enrollment.Put)
	assertions.Equal("getPersonBriefEnrollment", enrollment.Get.OperationID)
	assertions.Equal("setPersonBriefEnrollment", enrollment.Put.OperationID)

	assertions.Equal("2.20.0", APISchemaVersion)

	// The handlers accept an absent reason and an absent track, so the schema
	// generated clients are built from must not demand either.
	schemas := document.Components.Schemas.Map()
	rejectBody := schemas["RejectPersonBriefRequest"]
	requirements.NotNil(rejectBody)
	assertions.Contains(rejectBody.Properties, "reason")
	assertions.NotContains(rejectBody.Required, "reason")
	enrollmentBody := schemas["PutPersonBriefEnrollmentRequest"]
	requirements.NotNil(enrollmentBody)
	assertions.Equal([]string{"enrolled"}, enrollmentBody.Required)
	assertions.Contains(enrollmentBody.Properties, "track")

	// The join key is part of the contract generated clients are built from.
	briefSchema := schemas["PersonBrief"]
	requirements.NotNil(briefSchema)
	assertions.Contains(briefSchema.Properties, "renderer_policy")
	sentenceSchema := schemas["PersonBriefSentence"]
	requirements.NotNil(sentenceSchema)
	assertions.Contains(sentenceSchema.Properties, "evidence_ordinals")
}

// TestPersonBriefIsReachableOnlyThroughItsOwnRoutes pins the design's boundary:
// the brief is derived text about a third party and is exposed only where the
// owner asked for it. Nothing else in the HTTP contract may return it, which is
// what keeps it out of person search results and the CardDAV projection.
func TestPersonBriefIsReachableOnlyThroughItsOwnRoutes(t *testing.T) {
	briefSchemas := []string{
		`"#/components/schemas/PersonBrief"`,
		`"#/components/schemas/PersonBriefVersionsResponse"`,
	}
	allowed := map[string]bool{
		"/api/v1/people/{id}/brief":          true,
		"/api/v1/people/{id}/brief/versions": true,
		"/api/v1/people/{id}/brief/reject":   true,
	}
	require := require.New(t)
	assert := assert.New(t)

	document := OpenAPIDocument()
	encoded, err := json.Marshal(document.Paths)
	require.NoError(err)
	var paths map[string]json.RawMessage
	require.NoError(json.Unmarshal(encoded, &paths))
	for path, definition := range paths {
		if allowed[path] {
			continue
		}
		for _, schema := range briefSchemas {
			assert.NotContains(string(definition), schema,
				path+" must not return a person brief")
		}
	}
	for path := range allowed {
		assert.Contains(paths, path)
	}
}
