package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonCardOwnerBeforeAnyRead(t *testing.T) {
	// A nil store makes a mistaken pre-authorization read observable as a panic.
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	for _, mode := range []AuthMode{AuthModeDelegated, AuthModeRemoteClient, AuthModeSession, AuthModeLoopback} {
		t.Run(string(mode), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/people/email:alex@example.com/card", nil)
			req.SetPathValue("ref", "email:alex@example.com")
			req = req.WithContext(context.WithValue(req.Context(), requestSecurityContextKey{}, requestSecurity{auth: requestAuthentication{Mode: mode}}))
			w := httptest.NewRecorder()
			srv.handleGetPersonCard(w, req)
			assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.JSONEq(t, `{"error":"ref_not_permitted","message":"Person cards require owner access"}`, w.Body.String())
		})
	}
}

func TestPersonCardHTTPParametersAndStatuses(t *testing.T) {
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	srv.clock = func() time.Time { return time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		ref, query string
		code       int
		contains   string
	}{
		{strconv.FormatInt(person.ID, 10), "", 200, `"status":"ok"`},
		{fmt.Sprintf("person:%d", person.ID), "format=markdown", 200, `"format":"markdown"`},
		{"email:nobody@example.com", "", 200, `"status":"unknown"`},
		{"person:9999999", "", 404, `person_not_found`},
		{strconv.FormatInt(person.ID, 10), "max_items=0", 400, `invalid_max_items`},
		{"email:invalid", "", 400, `invalid_ref`},
		{"email:Alex <alex@example.com>", "", 400, `invalid_ref`},
		{"participant:0", "", 400, `invalid_ref`},
	} {
		t.Run(tc.ref+"?"+tc.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/people/"+url.PathEscape(tc.ref)+"/card?"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer owner")
			w := httptest.NewRecorder()
			srv.Handler(nil).ServeHTTP(w, req)
			require.Equal(t, tc.code, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.contains)
			if tc.code == http.StatusOK {
				assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			}
		})
	}
}

func TestPersonCardDelegatedHTTPDeniedBeforeStore(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	_, token, _, err := reg.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "owner@example.com"}}, time.Time{})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/people/email:alex@example.com/card", nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "ref_not_permitted")
}

func TestPersonCardRefResolutionAndEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	ctx := t.Context()
	pid, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
	requirements.NoError(err)
	observed, err := st.EnsureParticipant("observed@example.com", "Observed Example", "example.com")
	requirements.NoError(err)
	p, _, err := st.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	_, err = st.AddPersonContactPointContext(ctx, p.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	now := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	srv.clock = func() time.Time { return now }
	for _, tc := range []struct {
		ref, status string
		id          *int64
	}{{"email:alex@example.com", "ok", nil}, {fmt.Sprintf("participant:%d", pid), "ok", nil}, {"email:observed@example.com", "observed_only", &observed}, {fmt.Sprintf("participant:%d", observed), "observed_only", &observed}, {"participant:999999", "unknown", nil}} {
		t.Run(tc.ref, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/people/"+url.PathEscape(tc.ref)+"/card", nil)
			w := httptest.NewRecorder()
			srv.Handler(nil).ServeHTTP(w, req)
			requirements.Equal(200, w.Code, w.Body.String())
			var result personcard.PersonCardResult
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &result))
			assertions.Equal(tc.status, result.Status)
			assertions.Equal(tc.id, result.ParticipantID)
			assertions.Equal(now, result.CollectedAt)
			assertions.Equal(now, result.AsOf)
			if tc.status == "ok" {
				requirements.NotNil(result.Card)
				assertions.Contains(result.Gaps, "open_loops_unavailable")
				assertions.Contains(result.Gaps, "agenda_not_configured")
				assertions.Equal(p.ID, result.Card.Person.PersonID)
				encoded, err := json.Marshal(result.Card)
				requirements.NoError(err)
				assertions.Equal(len(encoded), result.ContentBytes)
			} else {
				assertions.Nil(result.Card)
			}
		})
	}
	otherID, err := st.EnsureParticipant("other@example.com", "Other Example", "example.com")
	requirements.NoError(err)
	other, _, err := st.CreatePersonFromParticipant(otherID)
	requirements.NoError(err)
	_, err = st.AddPersonContactPointContext(ctx, other.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	w := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/people/email:alex@example.com/card", nil))
	requirements.Equal(200, w.Code, w.Body.String())
	var result personcard.PersonCardResult
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &result))
	assertions.Equal("ambiguous", result.Status)
	requirements.Len(result.Candidates, 2)
	assertions.Equal(p.ID, result.Candidates[0].PersonID)
	assertions.Equal("source_controlled", result.Candidates[0].ContentTrust)
}

type unavailablePersonCardInputs struct{ *store.Store }

var errCardInput = errors.New("synthetic optional read failure")

func (b *unavailablePersonCardInputs) ContactStateContext(context.Context, int64, time.Time) (store.ContactState, error) {
	return store.ContactState{}, errCardInput
}
func (b *unavailablePersonCardInputs) GetPersonBriefContext(context.Context, int64, int) (*store.PersonBrief, error) {
	return nil, errCardInput
}
func (b *unavailablePersonCardInputs) ListMeetingActionsContext(context.Context, store.MeetingActionsQuery) (*meetingcontent.ActionsPage, error) {
	return nil, errCardInput
}
func (b *unavailablePersonCardInputs) ListPersonMeetingsContext(context.Context, store.SourceScope, store.PersonMeetingsQuery) ([]store.MeetingRecordSummary, string, error) {
	return nil, "", errCardInput
}
func (b *unavailablePersonCardInputs) BuildPersonFactCatalogContext(context.Context, bool) (personfacts.Catalog, error) {
	return personfacts.Catalog{}, errCardInput
}
func (b *unavailablePersonCardInputs) ListPersonSourcesContext(context.Context, int64, int) ([]store.PersonSource, error) {
	return nil, errCardInput
}

type pendingPersonCardInputs struct{ *store.Store }

func (b *pendingPersonCardInputs) ContactStateContext(_ context.Context, _ int64, _ time.Time) (store.ContactState, error) {
	return store.ContactState{Stale: true}, nil
}

func TestPersonCardRetainsPendingActivityStaleness(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("pending@example.test", "Pending Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}},
		Store:  &pendingPersonCardInputs{Store: st}, Logger: testLogger(),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/people/%d/card", person.ID), nil)
	req.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(w, req)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var result personcard.PersonCardResult
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &result))
	requirements.NotNil(result.Card)
	assertions.True(result.Freshness.Stale)
	assertions.NotContains(result.Gaps, "contact_state_none")
	requirements.NotNil(result.Card.LastInteraction)
	assertions.True(result.Card.LastInteraction.ProjectionStale)
}

func TestPersonCardOptionalFailuresPreserveProfile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	pid, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
	requirements.NoError(err)
	p, _, err := st.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &unavailablePersonCardInputs{Store: st}, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/people/%d/card", p.ID), nil)
	w := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(w, req)
	requirements.Equal(200, w.Code, w.Body.String())
	var result personcard.PersonCardResult
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &result))
	assertions.Equal("ok", result.Status)
	requirements.NotNil(result.Card)
	for _, gap := range []string{"contact_state_unavailable", "brief_unavailable", "actions_unavailable", "meetings_unavailable", "facts_unavailable", "sources_unavailable"} {
		assertions.Contains(result.Gaps, gap)
	}
	assertions.True(result.Freshness.Stale)
	assertions.NotContains(result.Gaps, "brief_none")
}

func TestPersonCardFactJoinUsesSupportedHistoryAndCurrentSensitivity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, st, id, _ := newPersonFactAPIFixture(t)
	now := time.Now().UTC().Add(time.Minute)
	read := func() personcard.Card {
		in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true}}
		srv.personCardFacts(t.Context(), id, &in, personcard.Options{AsOf: now})
		return personcard.Assemble(in, personcard.Options{AsOf: now})
	}
	first := read()
	requirements.Len(first.Items, 1)
	assertions.Equal("chat", first.Items[0].UntrustedText.Text)
	assertions.Equal("fact_decision", first.Items[0].Cite.Kind)
	assertions.Empty(first.Items[0].Cite.EvidenceRef)
	// Definition edits change the current catalog revision; the applied history
	// remains relevant, but current sensitivity must still win.
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE attribute_definitions SET description = ? WHERE slug = ?`), "Updated description", store.AttributeSlugPrimaryChannel)
	requirements.NoError(err)
	requirements.Len(read().Items, 1)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE attribute_definitions SET is_sensitive = TRUE WHERE slug = ?`), store.AttributeSlugPrimaryChannel)
	requirements.NoError(err)
	assertions.Empty(read().Items)
}

func TestPersonCardFactUsesProjectionTransitionTimeAndSkipsUnchangedSupport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, id, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target := targets[store.AttributeSlugPrimaryChannel]
	applyPersonFactAPIGeneration(t, st, id, target, "fact-transition-first", `"chat"`)
	firstDecisions, err := st.ListPersonFactDecisionsContext(t.Context(), id, personfacts.DecisionFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(firstDecisions, 1)
	requirements.NotNil(firstDecisions[0].Projection)

	applyPersonFactAPIGenerationWithRelation(t, st, id, target, "fact-transition-repeat", `"chat"`,
		personfacts.RelationSupport, 900, personFactAPINow.Add(24*time.Hour))
	decisions, err := st.ListPersonFactDecisionsContext(t.Context(), id, personfacts.DecisionFilter{Limit: 10})
	requirements.NoError(err)
	requirements.NotEmpty(decisions)
	unchangedApplied := false
	for _, decision := range decisions {
		unchangedApplied = unchangedApplied || decision.Action == personfacts.DecisionApplied && decision.Projection == nil
	}
	requirements.True(unchangedApplied)

	now := time.Now().UTC().Add(time.Minute)
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true}}
	srv.personCardFacts(t.Context(), id, &in, personcard.Options{AsOf: now})
	requirements.Len(in.Facts, 1)
	assertions.Equal(firstDecisions[0].ID, in.Facts[0].ID)
	assertions.Equal(personFactAPINow, in.Facts[0].ChangedAt)
}

func TestPersonCardFactFiltersNonTransitionsBeforeDecisionLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, id, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target := targets[store.AttributeSlugPrimaryChannel]
	applyPersonFactAPIGeneration(t, st, id, target, "fact-page-transition", `"chat"`)
	firstPage, err := st.ListPersonFactDecisionsContext(t.Context(), id, personfacts.DecisionFilter{Limit: 100})
	requirements.NoError(err)
	requirements.Len(firstPage, 1)
	requirements.NotNil(firstPage[0].Projection)

	for i := range 51 {
		applyPersonFactAPIGenerationWithRelation(t, st, id, target,
			fmt.Sprintf("fact-page-unchanged-%02d", i), `"chat"`,
			personfacts.RelationSupport, 900, personFactAPINow.Add(time.Duration(i+1)*time.Minute))
	}

	now := personFactAPINow.Add(60 * 24 * time.Hour)
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true}}
	srv.personCardFacts(t.Context(), id, &in, personcard.Options{AsOf: now})
	requirements.Len(in.Facts, 1, "a transition remains visible after newer unchanged decisions")
	assertions.Equal(firstPage[0].ID, in.Facts[0].ID)
	assertions.Equal(personFactAPINow, in.Facts[0].ChangedAt)
}

func TestPersonCardFactHydratesSelectedClaimBeyondTargetPages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, id, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target := targets[store.AttributeSlugPrimaryChannel]
	applyPersonFactAPIGeneration(t, st, id, target, "fact-page-original-transition", `"chat"`)

	targetRef := personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
	initialClaims, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{
		Target: &targetRef, AnyRevision: true, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(initialClaims, 1)
	requirements.Len(initialClaims[0].EvidenceIDs, 1)
	originalClaimKey := initialClaims[0].ClaimKey
	originalEvidenceID := initialClaims[0].EvidenceIDs[0]

	claims := make([]personfacts.ProposedClaim, 201)
	for i := range claims {
		resolvedAt := personFactAPINow.Add(time.Duration(i+1) * time.Minute)
		claims[i] = personfacts.ProposedClaim{
			Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(fmt.Sprintf(`"rejected-%03d"`, i)),
			Origin:         personfacts.OriginExtraction,
			Confidence:     personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				PersonID: id, SourceClass: personfacts.EvidencePublic,
				Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
				SourceURL:       fmt.Sprintf("https://example.test/rejected/%03d", i),
				SubjectPersonID: &id, SubjectRef: "synthetic-person",
				Excerpt:       fmt.Sprintf("Synthetic rejected observation %03d", i),
				SourceVersion: "source-v1", EventTime: resolvedAt.Add(-time.Hour),
				RecordedTime: resolvedAt, IdentityScore: 990,
			}},
		}
	}
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: id,
		SourceCursors: []personfacts.SourceCursor{{
			Lane: "fixture", Start: "fact-page-rejected-bulk", End: "fact-page-rejected-bulk-end",
		}},
		ProgramID: "fixture-program", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture-catalog",
		Provider: "fixture-provider-secret", ProviderVersion: "v1",
		Model: "fixture-model", ModelVersion: "v1", ResolvedAt: personFactAPINow.Add(24 * time.Hour),
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		Claims: claims,
	}, nil)
	requirements.NoError(err)

	claimPage, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{
		Target: &targetRef, AnyRevision: true, Limit: 200,
	})
	requirements.NoError(err)
	requirements.Len(claimPage, 200)
	for _, claim := range claimPage {
		assertions.NotEqual(originalClaimKey, claim.ClaimKey)
	}
	evidencePage, err := st.ListPersonFactEvidenceContext(t.Context(), id, personfacts.EvidenceFilter{
		Target: &targetRef, AnyRevision: true, Limit: 200,
	})
	requirements.NoError(err)
	requirements.Len(evidencePage, 200)
	for _, evidence := range evidencePage {
		assertions.NotEqual(originalEvidenceID, evidence.ID)
	}

	now := personFactAPINow.Add(24 * time.Hour)
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true}}
	srv.personCardFacts(t.Context(), id, &in, personcard.Options{AsOf: now})
	requirements.Len(in.Facts, 1, "the recent transition remains visible after 200 newer claims and evidence rows")
	assertions.Equal(personFactAPINow, in.Facts[0].ChangedAt)
}

func TestPersonCardFactRendersRetiredRelation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, id, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target := targets[store.AttributeSlugPrimaryChannel]
	applyPersonFactAPIGenerationWithRelation(t, st, id, target, "fact-retirement-first", `"chat"`,
		personfacts.RelationSupport, 0, personFactAPINow)
	applyPersonFactAPIGenerationWithRelation(t, st, id, target, "fact-retirement-supersede", `"chat"`,
		personfacts.RelationSupersede, 1000, personFactAPINow.Add(time.Hour))
	decisions, err := st.ListPersonFactDecisionsContext(t.Context(), id, personfacts.DecisionFilter{Limit: 10})
	requirements.NoError(err)
	requirements.NotEmpty(decisions)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{
		Target:      &personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
		AnyRevision: true, Limit: 10,
	})
	requirements.NoError(err)
	var supersedeKey string
	for _, claim := range claims {
		if claim.Relation == personfacts.RelationSupersede {
			supersedeKey = claim.ClaimKey
			break
		}
	}
	requirements.NotEmpty(supersedeKey)
	foundAppliedRetirement := false
	for _, decision := range decisions {
		if decision.ClaimKey == supersedeKey && decision.Action == personfacts.DecisionApplied && decision.Projection != nil {
			foundAppliedRetirement = true
		}
	}
	requirements.True(foundAppliedRetirement)

	now := time.Now().UTC().Add(time.Minute)
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true}}
	opts := personcard.Options{AsOf: now}
	srv.personCardFacts(t.Context(), id, &in, opts)
	var retiredFact *personcard.FactChange
	for i := range in.Facts {
		if in.Facts[i].Relation == string(personfacts.RelationSupersede) {
			retiredFact = &in.Facts[i]
			break
		}
	}
	requirements.NotNil(retiredFact)
	card := personcard.Assemble(personcard.Inputs{
		CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true},
		Facts: []personcard.FactChange{*retiredFact},
	}, opts)
	requirements.Len(card.Items, 1)
	assertions.Equal(string(personfacts.RelationSupersede), card.Items[0].Relation)
	assertions.Contains(personcard.RenderMarkdown(card, "external"), "retired (superseded)")
}

func TestPersonCardFactJoinBatchesEvidenceSummaries(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	srv, st, id, targets := newPersonFactAPIFixtureWithoutClaims(t)
	stubStore, ok := srv.store.(*stubIdentityCacheStore)
	requirements.True(ok)
	spy := &personCardSummarySpy{stubIdentityCacheStore: stubStore}
	srv.store = spy
	requirements.Contains(targets, store.AttributeSlugLocation)
	subject := id
	evidence := func(ref, suffix string, eventTime time.Time) personfacts.EvidenceInput {
		return personfacts.EvidenceInput{
			PersonID: id, SourceClass: personfacts.EvidencePublic,
			Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
			SourceRef: ref, SourceURL: "https://example.test/" + suffix,
			SubjectPersonID: &subject, SubjectRef: "synthetic-person",
			Excerpt: "Synthetic archive payload " + suffix, SourceVersion: "source-v1",
			EventTime: eventTime, RecordedTime: personFactAPINow, IdentityScore: 990,
		}
	}
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: id,
		SourceCursors: []personfacts.SourceCursor{{
			Lane: "fixture", Start: "batch-summary", End: "batch-summary-end",
		}},
		ProgramID: "fixture-program", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture-catalog",
		Provider: "fixture-provider-secret", ProviderVersion: "v1",
		Model: "fixture-model", ModelVersion: "v1", ResolvedAt: personFactAPINow,
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		Claims: []personfacts.ProposedClaim{
			{
				Target: targets[store.AttributeSlugPrimaryChannel], Relation: personfacts.RelationSupport,
				SubmittedValue: json.RawMessage(`"chat"`), Origin: personfacts.OriginExtraction,
				Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
				Evidence: []personfacts.EvidenceInput{
					evidence("message:101", "101", personFactAPINow.Add(-time.Hour)),
				},
			},
			{
				Target: targets[store.AttributeSlugLocation], Relation: personfacts.RelationSupport,
				SubmittedValue: json.RawMessage(`"Lisbon"`), Origin: personfacts.OriginExtraction,
				Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
				Evidence: []personfacts.EvidenceInput{
					evidence("message:102", "102", personFactAPINow.Add(-2*time.Hour)),
				},
			},
		},
	}, nil)
	requirements.NoError(err)

	now := time.Now().UTC().Add(time.Minute)
	var in personcard.Inputs
	srv.personCardFacts(t.Context(), id, &in, personcard.Options{AsOf: now})
	requirements.Len(in.Facts, 2)
	requirements.Len(spy.summaryCalls, 1)
	assertions.ElementsMatch([]int64{101, 102}, spy.summaryCalls[0])
	sourceIDs := make([]int64, 0, len(in.Facts))
	for _, fact := range in.Facts {
		sourceIDs = append(sourceIDs, fact.SourceIDs...)
	}
	assertions.ElementsMatch([]int64{1101, 1102}, sourceIDs)
}

type personCardSummarySpy struct {
	*stubIdentityCacheStore

	summaryCalls [][]int64
}

func (s *personCardSummarySpy) GetMessagesSummariesByIDsContext(_ context.Context, ids []int64) ([]APIMessage, error) {
	s.summaryCalls = append(s.summaryCalls, append([]int64(nil), ids...))
	summaries := make([]APIMessage, 0, len(ids))
	for _, id := range ids {
		summaries = append(summaries, APIMessage{ID: id, SourceID: id + 1000})
	}
	return summaries, nil
}

func TestPersonCardFactTiePrefersCurrentUserEdit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, st, id, _ := newPersonFactAPIFixture(t)
	value := "email"
	_, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	tie := personFactAPINow
	now := tie.Add(time.Hour)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_attribute_values SET created_at = ? WHERE person_id = ? AND source = 'user'`), tie, id)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_resolutions SET resolved_at = ? WHERE person_id = ?`), tie, id)
	requirements.NoError(err)
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true}}
	srv.personCardFacts(t.Context(), id, &in, personcard.Options{AsOf: now})
	requirements.Len(in.Facts, 2)
	var factTime, userTime time.Time
	for _, fact := range in.Facts {
		if fact.User {
			userTime = fact.ChangedAt
		} else {
			factTime = fact.ChangedAt
		}
	}
	assertions.Equal(tie, factTime)
	assertions.Equal(tie, userTime)
	assertions.Equal(factTime, userTime)
	card := personcard.Assemble(in, personcard.Options{AsOf: now})
	requirements.Len(card.Items, 1)
	assertions.Equal("user", card.Items[0].ContentTrust)
	assertions.Equal("email", card.Items[0].UntrustedText.Text)
	assertions.Equal("attribute_value", card.Items[0].Cite.Kind)
}

type failedCardProfile struct{ *store.Store }

func (b *failedCardProfile) GetPersonTrackingContext(context.Context, int64) (*store.PersonTracking, error) {
	return nil, errCardInput
}
func TestPersonCardRequiredProfileFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	pid, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
	requirements.NoError(err)
	p, _, err := st.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &failedCardProfile{Store: st}, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	w := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/people/%d/card", p.ID), nil))
	assertions.Equal(500, w.Code, w.Body.String())
	assertions.NotContains(w.Body.String(), "not_tracked")
}

func TestPersonCardEventFactUsesStaleSourceAndCollectionClock(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, st, id, _ := newPersonFactAPIFixture(t)
	now := time.Now().UTC().Add(time.Minute)
	p, err := st.GetPersonContext(t.Context(), id)
	requirements.NoError(err)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "event", "Planning")
	requirements.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "event", MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true}})
	requirements.NoError(err)
	ref := fmt.Sprintf("evt:%d", mid)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_evidence SET source_ref = ? WHERE person_id = ?`), ref, id)
	requirements.NoError(err)
	_, err = st.RecordContactObservationContext(t.Context(), p.ParticipantIDs[0], store.ParticipantContactObservationInput{SourceID: &source.ID, AddressKind: store.ContactAddressEmail, OriginalValue: "facts@example.test", ObservedAt: &now, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
	requirements.NoError(err)
	opts := personcard.Options{AsOf: now.AddDate(0, 1, 0)}
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: id, Tracked: true}}
	srv.personCardFacts(t.Context(), id, &in, opts)
	srv.personCardSources(t.Context(), id, &in)
	card := personcard.Assemble(in, opts)
	requirements.Len(card.Items, 1)
	assertions.True(card.Items[0].Stale)
	assertions.Equal(ref, card.Items[0].Cite.EvidenceRef)
	requirements.Len(card.Freshness.Sources, 1)
	assertions.Nil(card.Freshness.Sources[0].LastSuccessfulSyncAt)
	assertions.Empty(card.Freshness.Sources[0].SyncCommand)
	run, err := st.StartSync(source.ID, "full")
	requirements.NoError(err)
	requirements.NoError(st.CompleteSync(run, "done"))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sync_runs SET completed_at = ? WHERE id = ?`), now.Add(-time.Hour), run)
	requirements.NoError(err)
	in.Freshness = personcard.FreshnessInputs{}
	srv.personCardSources(t.Context(), id, &in)
	card = personcard.Assemble(in, opts)
	requirements.Len(card.Freshness.Sources, 1)
	age := card.Freshness.Sources[0].AgeSeconds
	requirements.NotNil(age)
	assertions.Equal(int64(3600), *age)
	assertions.False(card.Items[0].Stale)
}

type populatedCardInputs struct {
	*store.Store

	actions       *meetingcontent.ActionsPage
	meetings      []store.MeetingRecordSummary
	meetingRecord store.MeetingRecord
	meetingQuery  store.PersonMeetingsQuery
}

func (b *populatedCardInputs) ListMeetingActionsContext(context.Context, store.MeetingActionsQuery) (*meetingcontent.ActionsPage, error) {
	return b.actions, nil
}
func (b *populatedCardInputs) ListPersonMeetingsContext(_ context.Context, _ store.SourceScope, q store.PersonMeetingsQuery) ([]store.MeetingRecordSummary, string, error) {
	b.meetingQuery = q
	return b.meetings, "", nil
}
func (b *populatedCardInputs) GetMeetingRecordContext(context.Context, store.SourceScope, string) (store.MeetingRecord, error) {
	return b.meetingRecord, nil
}

func personCardTestPacket(t *testing.T, sourceType string, content meetingcontent.Content) *meetingcontent.Packet {
	t.Helper()
	result, err := meetingcontent.Render("archive", []meetingcontent.Entry{{
		Meeting: meetingcontent.MeetingRef{MessageID: 73, SourceType: sourceType}, Content: content,
	}}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 1 << 20})
	require.NoError(t, err)
	packet, err := meetingcontent.PacketFromResult(result)
	require.NoError(t, err)
	return packet
}

func TestPersonCardActionOwnerAndMeetingEvidenceAdapters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	pid, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
	requirements.NoError(err)
	p, _, err := st.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	now := time.Now().UTC()
	at := now.Add(-time.Hour)
	participantID := pid
	packet := personCardTestPacket(t, "granola", meetingcontent.Content{SourceParticipants: []meetingcontent.Participant{{
		ParticipantID: &participantID, Email: "alex@example.com", Role: "to", Provenance: "provider_attendee",
	}}})
	// A fake bounded reader supplies already-parsed rows; the production adapter
	// and projection classify identity and evidence, which is this regression.
	inputs := &populatedCardInputs{
		Store: st,
		actions: &meetingcontent.ActionsPage{Rows: []meetingcontent.ActionRow{
			{Meeting: meetingcontent.MeetingRef{MessageID: 1, SourceID: 1, SourceType: "granola", SourceIdentifier: "meeting-account", OccurredAt: &at}, Action: meetingcontent.Action{Ordinal: 1, Status: meetingcontent.StatusPending, Title: "Send agenda", AssigneeEmail: "owner@example.com"}},
			{Meeting: meetingcontent.MeetingRef{MessageID: 1, SourceID: 1, SourceType: "granola", SourceIdentifier: "meeting-account", OccurredAt: &at}, Action: meetingcontent.Action{Ordinal: 2, Status: meetingcontent.StatusPending, Title: "Review notes", AssigneeEmail: "alex@example.com"}},
		}},
		meetings: []store.MeetingRecordSummary{{Ref: "evt:2", Start: at, ItemCount: 1, HasTranscript: true, Occurrence: &store.CalendarOccurrence{Attendees: []calevent.Attendee{{Email: "alex@example.com"}}}}},
		meetingRecord: store.MeetingRecord{
			Participants: []store.MeetingParticipant{{ParticipantID: pid, Email: "alex@example.com", Provenance: "provider_attendee"}},
			Items:        []store.MeetingItem{{Content: packet}},
		},
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Granola: []config.GranolaSource{{Identifier: "meeting-account", AccountEmail: "owner@example.com"}}}, Store: inputs, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: p.ID, Tracked: true}}
	opts := personcard.Options{AsOf: now}
	srv.personCardActions(t.Context(), p, &in)
	requirements.Len(in.Actions, 2)
	assertions.Equal("owner", in.Actions[0].AssigneeClass)
	assertions.Equal("person", in.Actions[1].AssigneeClass)
	srv.personCardMeetings(t.Context(), p, &in, opts)
	assertions.True(inputs.meetingQuery.NewestFirst)
	card := personcard.Assemble(in, opts)
	requirements.Len(card.Items, 2)
	classes := []string{card.Items[0].AssigneeClass, card.Items[1].AssigneeClass}
	assertions.ElementsMatch([]string{"owner", "person"}, classes)
	assertions.Equal(1, card.Meetings.CountByBasis["transcript"])
	assertions.Equal(&at, card.Meetings.LastMetAt)
	assertions.Equal(&at, card.Meetings.LastInvitedAt)
}

func TestPersonCardMeetingBasisUsesParticipantProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, rawFormat, raw string
		provenance, wantPacketProvenance string
		wantBasis                        string
		storeDefaultsProvenance          bool
		wantLastMet                      bool
	}{
		{name: "invitation only", sourceType: "granola", provenance: "invited", wantPacketProvenance: "invited", wantBasis: "invited"},
		{name: "provider participant", sourceType: "granola", provenance: "provider_attendee", wantPacketProvenance: "provider_attendee", wantBasis: "transcript", wantLastMet: true},
		{
			name: "Twenty calendar participant", sourceType: "twenty", rawFormat: "twenty_json",
			raw:        `{"schema_version":1,"recording":{"summary":{"markdown":"Meeting summary"}},"participants":[{"handle":"alex@example.com","displayName":"Alex Example"}]}`,
			provenance: "invited", wantPacketProvenance: "invited", storeDefaultsProvenance: true, wantBasis: "invited",
		},
		{
			name: "Notion calendar participant", sourceType: "notion_meetings", rawFormat: "notion_meeting_json",
			raw:        `{"schema_version":1,"discovery":{"meeting_notes":{"calendar_event":{"attendees":["user-1"]}}},"canonical":{"summary":"Meeting summary"},"attendee_labels":["Alex Example"],"resolved_users":[{"id":"user-1","name":"Alex Example","email":"alex@example.com","email_verified":true}]}`,
			provenance: "invited", wantPacketProvenance: "invited", storeDefaultsProvenance: true, wantBasis: "invited",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := testutil.NewTestStore(t)
			participantID, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participantID)
			requirements.NoError(err)
			now := time.Now().UTC()
			at := now.Add(-time.Hour)
			content := meetingcontent.Content{SourceParticipants: []meetingcontent.Participant{{Email: "alex@example.com", Role: "to", Provenance: tc.provenance}}}
			if tc.raw != "" {
				content = meetingcontent.Decode(tc.rawFormat, []byte(tc.raw), nil)
			}
			packet := personCardTestPacket(t, tc.sourceType, content)
			requirements.Len(packet.Meetings, 1)
			requirements.Len(packet.Meetings[0].Participants, 1)
			assertions.Equal(tc.wantPacketProvenance, packet.Meetings[0].Participants[0].Provenance)
			aggregateProvenance := tc.provenance
			if tc.storeDefaultsProvenance {
				aggregateProvenance = "provider_attendee"
			}
			inputs := &populatedCardInputs{
				Store: st,
				meetings: []store.MeetingRecordSummary{{
					Ref: "evt:2", Start: at, ItemCount: 1, HasTranscript: true,
					Occurrence: &store.CalendarOccurrence{Attendees: []calevent.Attendee{{Email: "alex@example.com"}}},
				}},
				meetingRecord: store.MeetingRecord{
					Participants: []store.MeetingParticipant{{ParticipantID: participantID, Email: "alex@example.com", Provenance: aggregateProvenance}},
					Items:        []store.MeetingItem{{Content: packet}},
				},
			}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: inputs, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: person.ID, Emails: []string{"alex@example.com"}}}
			opts := personcard.Options{AsOf: now}

			srv.personCardMeetings(t.Context(), person, &in, opts)
			card := personcard.Assemble(in, opts)

			requirements.Len(card.Meetings.Recent, 1)
			assertions.Equal(tc.wantBasis, card.Meetings.Recent[0].Basis)
			assertions.Equal(tc.wantLastMet, card.Meetings.LastMetAt != nil)
			requirements.NotNil(card.Meetings.LastInvitedAt)
			assertions.Equal(at, *card.Meetings.LastInvitedAt)
		})
	}
}

func TestPersonCardDecodedProviderParticipantsCountAsParticipation(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, rawFormat, raw string
		linkedEvent                      bool
	}{
		{
			name:       "Circleback standalone note",
			sourceType: "circleback", rawFormat: "circleback_json",
			raw: `{"meeting":{"notes":"Release notes","attendees":[{"name":"Alex Example","email":"alex@example.com"}]},"transcript":{"transcript":[{"speakerName":"Alex Example","content":"Shipped"}]}}`,
		},
		{
			name:       "Muesli calendar-linked note",
			sourceType: "muesli", rawFormat: "muesli_json", linkedEvent: true,
			raw: `{"meeting":{"raw_transcript":"Alex Example: Shipped"},"participants":[{"name":"Alex Example","email":"alex@example.com","source":"manual"}]}`,
		},
		{
			name:       "generic standalone note",
			sourceType: "meeting_import", rawFormat: "meeting_json",
			raw: `{"summary_markdown":"Release notes","transcript":"Alex Example: Shipped","attendees":[{"name":"Alex Example","email":"alex@example.com"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			content := meetingcontent.Decode(tc.rawFormat, []byte(tc.raw), nil)
			requirements.Len(content.SourceParticipants, 1)
			assertions.Empty(content.SourceParticipants[0].Provenance)
			packet := personCardTestPacket(t, tc.sourceType, content)
			requirements.Len(packet.Meetings, 1)
			requirements.Len(packet.Meetings[0].Participants, 1)
			assertions.Empty(packet.Meetings[0].Participants[0].Provenance)

			st := testutil.NewTestStore(t)
			participantID, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.org")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participantID)
			requirements.NoError(err)
			now := time.Now().UTC()
			at := now.Add(-time.Hour)
			row := store.MeetingRecordSummary{Ref: "msg:73", Start: at, ItemCount: 1, HasTranscript: true}
			if tc.linkedEvent {
				row.Ref = "evt:2"
				row.Occurrence = &store.CalendarOccurrence{Attendees: []calevent.Attendee{{Email: "alex@example.com"}}}
			}
			inputs := &populatedCardInputs{
				Store:    st,
				meetings: []store.MeetingRecordSummary{row},
				meetingRecord: store.MeetingRecord{
					Items: []store.MeetingItem{{Content: packet}},
				},
			}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: inputs, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: person.ID, Emails: []string{"alex@example.com"}}}
			opts := personcard.Options{AsOf: now}

			srv.personCardMeetings(t.Context(), person, &in, opts)
			card := personcard.Assemble(in, opts)

			requirements.Len(card.Meetings.Recent, 1)
			assertions.Equal("transcript", card.Meetings.Recent[0].Basis)
			requirements.NotNil(card.Meetings.LastMetAt)
			assertions.Equal(at, *card.Meetings.LastMetAt)
			if tc.linkedEvent {
				requirements.NotNil(card.Meetings.LastInvitedAt)
				assertions.Equal(at, *card.Meetings.LastInvitedAt)
			} else {
				assertions.Nil(card.Meetings.LastInvitedAt)
			}
		})
	}
}

func FuzzPersonCardMeetingParticipantStatusDefaultsUnspecifiedProvenance(f *testing.F) {
	for _, tc := range [][2]string{
		{"circleback", "Alex"},
		{"muesli", "Muesli participant"},
		{"meeting_import", "Imported guest"},
		{"twenty", "Calendar invitee"},
		{"notion_meetings", "Notion attendee"},
	} {
		f.Add(tc[0], tc[1])
	}
	f.Fuzz(func(t *testing.T, sourceType, localSeed string) {
		var local strings.Builder
		for _, r := range localSeed {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				local.WriteRune(r)
			}
		}
		if local.Len() == 0 {
			local.WriteByte('x')
		}
		email := strings.ToLower("person+" + local.String() + "@example.org")
		sourceType = strings.ToLower(strings.TrimSpace(sourceType))
		record := store.MeetingRecord{Items: []store.MeetingItem{{Content: &meetingcontent.Packet{Meetings: []meetingcontent.Entry{{
			Meeting:      meetingcontent.MeetingRef{SourceType: sourceType},
			Participants: []meetingcontent.Participant{{Email: " " + strings.ToUpper(email) + " "}},
		}}}}}}
		invited, participated := personCardMeetingParticipantStatus(record, nil, map[string]struct{}{email: {}})
		calendarParticipants := sourceType == "twenty" || sourceType == "notion_meetings"
		assert.Equal(t, calendarParticipants, invited)
		assert.Equal(t, !calendarParticipants, participated)
	})
}

func TestPersonCardMeetingReportsTruncatedParticipantEvidence(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	now := time.Now().UTC()
	at := now.Add(-time.Hour)
	items := make([]store.MeetingItem, 10)
	for i := range items {
		messageID := int64(100 + i)
		result, err := meetingcontent.Render("archive", []meetingcontent.Entry{{
			Meeting: meetingcontent.MeetingRef{MessageID: messageID, SourceType: "granola"},
			Content: meetingcontent.Content{
				Summary:    meetingcontent.Section{State: meetingcontent.StateEmpty},
				Notes:      meetingcontent.Section{State: meetingcontent.StateEmpty},
				Transcript: meetingcontent.Transcript{State: meetingcontent.StateEmpty},
				Actions:    []meetingcontent.Action{},
			},
		}}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 4096})
		requirements.NoError(err)
		packet, err := meetingcontent.PacketFromResult(result)
		requirements.NoError(err)
		items[i] = store.MeetingItem{MessageID: messageID, Content: packet}
	}

	for _, tc := range []struct {
		name      string
		itemCount int
		truncated bool
	}{{"complete", 10, false}, {"missing_item", 11, true}} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			inputs := &populatedCardInputs{
				Store: st,
				meetings: []store.MeetingRecordSummary{{
					Ref: "evt:2", Start: at, ItemCount: tc.itemCount,
					Occurrence: &store.CalendarOccurrence{Attendees: []calevent.Attendee{{Email: "alex@example.com"}}},
				}},
				meetingRecord: store.MeetingRecord{
					Items: items,
					Participants: []store.MeetingParticipant{{
						ParticipantID: participantID, Email: "alex@example.com", Provenance: "invited",
					}},
				},
			}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: inputs, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: person.ID, Tracked: true, Emails: []string{"alex@example.com"}}}
			srv.personCardMeetings(t.Context(), person, &in, personcard.Options{AsOf: now})

			requirements.Len(in.Meetings, 1)
			assertions.Equal("invited", in.Meetings[0].Basis)
			assertions.False(in.Meetings[0].Participated)
			assertions.Equal(tc.truncated, in.Coverage.MeetingsTruncated)
		})
	}
}

func TestPersonCardMeetingReportsOmittedPacketParticipantEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipant("alex@example.com", "Alex Example", "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	packetResult, err := meetingcontent.Render("archive", []meetingcontent.Entry{{
		Meeting: meetingcontent.MeetingRef{MessageID: 73, SourceType: "granola"},
		Content: meetingcontent.Content{
			SourceParticipants: []meetingcontent.Participant{{
				Email: "alex@example.com", Name: strings.Repeat("Synthetic Participant ", 500), Role: "to", Provenance: "invited",
			}},
			Summary:    meetingcontent.Section{State: meetingcontent.StateEmpty},
			Notes:      meetingcontent.Section{State: meetingcontent.StateEmpty},
			Transcript: meetingcontent.Transcript{State: meetingcontent.StateEmpty},
			Actions:    []meetingcontent.Action{},
		},
	}}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 1024})
	requirements.NoError(err)
	packet, err := meetingcontent.PacketFromResult(packetResult)
	requirements.NoError(err)
	requirements.Empty(packet.Meetings)
	requirements.Equal([]int64{73}, packet.OmittedMessageIDs)
	now := time.Now().UTC()
	at := now.Add(-time.Hour)
	inputs := &populatedCardInputs{
		Store: st,
		meetings: []store.MeetingRecordSummary{{
			Ref: "evt:2", Start: at, ItemCount: 1,
			Occurrence: &store.CalendarOccurrence{Attendees: []calevent.Attendee{{Email: "alex@example.com"}}},
		}},
		meetingRecord: store.MeetingRecord{Items: []store.MeetingItem{{
			MessageID: 73,
			Content:   packet,
		}}},
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: inputs, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	in := personcard.Inputs{CollectedAt: now, Person: personcard.Person{PersonID: person.ID, Tracked: true, Emails: []string{"alex@example.com"}}}
	srv.personCardMeetings(t.Context(), person, &in, personcard.Options{AsOf: now})

	requirements.Len(in.Meetings, 1)
	assertions.Equal("invited", in.Meetings[0].Basis)
	assertions.False(in.Meetings[0].Participated)
	assertions.True(in.Coverage.MeetingsTruncated)
}
