package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var factMCPToolNames = []string{
	"list_fact_targets", "list_person_fact_evidence", "list_person_fact_status_events", "list_person_fact_claims", "list_person_fact_decisions", "list_person_fact_pins", "set_person_fact_pin", "get_person_tracking", "set_person_tracking",
	"list_person_merges", "get_person_merge", "get_person_merge_snapshot", "review_person_merge_candidate", "split_person",
}

func TestMCPFactDiscoveryUsesActualOwningRoutes(t *testing.T) {
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t), config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	names := backend.capabilities()
	for _, name := range factMCPToolNames {
		assertions.True(slices.Contains(names, name), "missing native fact/recovery tool %s", name)
	}
}

func TestMCPFactTrackingFalseAndPinsUseNativeTargetRevision(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, approvals := recordSDKFixture(t)
	native, err := st.GetPersonTrackingContext(t.Context(), person.ID)
	requirements.NoError(err)
	current := callRecordTool[generated.PersonTracking](t, session, "get_person_tracking", map[string]any{"person_id": person.ID})
	assertions.Equal(native.Tracked, current.Tracked)
	enabled := callRecordTool[generated.PersonTracking](t, session, "set_person_tracking", map[string]any{"person_id": person.ID, "tracked": true})
	assertions.True(enabled.Tracked)
	requirements.NotNil(enabled.TrackedAt)
	catalog := callRecordTool[generated.Catalog](t, session, "list_fact_targets", map[string]any{})
	requirements.NotEmpty(catalog.Fingerprint)
	requirements.NotEmpty(catalog.Targets)
	var target *generated.TargetDescriptor
	for i := range catalog.Targets {
		if catalog.Targets[i].Slug == "primary_channel" {
			target = &catalog.Targets[i]
			break
		}
	}
	requirements.NotNil(target)
	pinned := callRecordTool[generated.PersonFactPinWrite](t, session, "set_person_fact_pin", map[string]any{"person_id": person.ID, "kind": target.Kind, "key": target.Key, "pinned": true})
	assertions.True(pinned.State.Pinned)
	assertions.Equal(target.Revision, pinned.State.Target.Revision, "daemon resolves active descriptor revision; no caller ETag")
	pins := callRecordTool[generated.PersonFactPinsResponse](t, session, "list_person_fact_pins", map[string]any{"person_id": person.ID})
	requirements.NotEmpty(pins.Pins)
	assertions.True(pins.Pins[0].Pinned)
	unpinned := callRecordTool[generated.PersonFactPinWrite](t, session, "set_person_fact_pin", map[string]any{"person_id": person.ID, "kind": target.Kind, "key": target.Key, "pinned": false})
	assertions.False(unpinned.State.Pinned)
	disabled := callRecordTool[generated.PersonTracking](t, session, "set_person_tracking", map[string]any{"person_id": person.ID, "tracked": false})
	assertions.False(disabled.Tracked)
	native, err = st.GetPersonTrackingContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.False(native.Tracked)
	count := *approvals
	for _, args := range []map[string]any{{"person_id": person.ID}, {"person_id": person.ID, "tracked": nil}, {"person_id": person.ID, "tracked": false, "env": "invented"}} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_tracking", Arguments: args})
		assertions.True(err != nil || result != nil && result.IsError)
		assertions.Equal(count, *approvals, "omission/null/unknown arguments refuse before approval")
	}
}

func TestMCPFactHistoryRetainsProvenanceAndUnsupportedStatus(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, _ := recordSDKFixture(t)
	_, err := st.SetPersonTrackingContext(t.Context(), person.ID, true)
	requirements.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	requirements.NoError(err)
	var target personfacts.TargetDescriptor
	for _, descriptor := range catalog.Targets {
		if descriptor.Slug == "primary_channel" {
			target = descriptor
			break
		}
	}
	requirements.NotEmpty(target.Key)
	now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: person.ID, SourceCursors: []personfacts.SourceCursor{{Lane: "synthetic", Start: "one", End: "two"}},
		ProgramID: "synthetic-program", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: catalog.Fingerprint,
		Provider: "synthetic-provider-secret", ProviderVersion: "v1", Model: "synthetic-model", ModelVersion: "v1", ResolvedAt: now,
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "synthetic-policy"},
		Claims: []personfacts.ProposedClaim{{Target: target, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"chat"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{{PersonID: person.ID, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/synthetic-evidence", SubjectPersonID: &person.ID, SubjectRef: "synthetic-person", Excerpt: "Synthetic private evidence excerpt", SourceVersion: "synthetic-source-v1", EventTime: now.Add(-time.Hour), RecordedTime: now, IdentityScore: 990}}}},
	}, nil)
	requirements.NoError(err)
	encoded, err := personfacts.EncodeTargetRef(personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision})
	requirements.NoError(err)
	ordinary := callRecordTool[generated.PersonFactEvidenceResponse](t, session, "list_person_fact_evidence", map[string]any{"person_id": person.ID, "target": encoded, "limit": 1, "offset": 0})
	requirements.Len(ordinary.Evidence, 1)
	assertions.Nil(ordinary.Evidence[0].Excerpt)
	assertions.Nil(ordinary.Evidence[0].SourceURL)
	assertions.Equal("synthetic-source-v1", *ordinary.Evidence[0].SourceVersion)
	explicit := callRecordTool[generated.PersonFactEvidenceResponse](t, session, "list_person_fact_evidence", map[string]any{"person_id": person.ID, "target": encoded, "fields": []any{"excerpt", "source_details"}, "include_sensitive": true})
	requirements.Len(explicit.Evidence, 1)
	requirements.NotNil(explicit.Evidence[0].Excerpt)
	assertions.Equal("Synthetic private evidence excerpt", *explicit.Evidence[0].Excerpt)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{PersonID: person.ID, SourceCursors: []personfacts.SourceCursor{{Lane: "synthetic-status", Start: "two", End: "three"}}, ProgramID: "synthetic-program", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: catalog.Fingerprint, Provider: "synthetic-provider-secret", ProviderVersion: "v1", ResolvedAt: now.Add(time.Minute), Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "synthetic-policy"}, EvidenceStatusChanges: []personfacts.EvidenceStatusChange{{EvidenceKey: ordinary.Evidence[0].EvidenceKey, SourceVersion: "synthetic-source-v1", Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}}}, nil)
	requirements.NoError(err)
	history := callRecordTool[generated.PersonFactEvidenceStatusEventsResponse](t, session, "list_person_fact_status_events", map[string]any{"person_id": person.ID, "evidence_key": ordinary.Evidence[0].EvidenceKey, "supported": false, "limit": 1, "offset": 0})
	requirements.Len(history.Events, 1)
	assertions.False(history.Events[0].Supported)
	assertions.Equal(string(personfacts.EvidenceStatusSourceDeleted), history.Events[0].Reason)
	claims := callRecordTool[mcpserver.FactClaimSelections](t, session, "list_person_fact_claims", map[string]any{"person_id": person.ID, "target": encoded, "limit": 1})
	requirements.Len(claims.Claims, 1)
	assertions.Equal("synthetic-program", claims.Claims[0].ProgramID)
	requirements.NotNil(claims.Claims[0].SubmittedValue)
	assertions.Equal(`"chat"`, *claims.Claims[0].SubmittedValue)
	decisions := callRecordTool[generated.PersonFactDecisionsResponse](t, session, "list_person_fact_decisions", map[string]any{"person_id": person.ID, "target": encoded, "limit": 1})
	requirements.NotEmpty(decisions.Decisions)
	data, err := json.Marshal(claims)
	requirements.NoError(err)
	assertions.NotContains(string(data), "synthetic-provider-secret")
	empty := callRecordTool[generated.PersonFactClaimsResponse](t, session, "list_person_fact_claims", map[string]any{"person_id": person.ID, "offset": 1000})
	assertions.Empty(empty.Claims)
	// Change real definition metadata after generation; the adapter must use
	// the current owner classification rather than infer sensitivity from values.
	_, err = st.DB().ExecContext(t.Context(), "UPDATE attribute_definitions SET is_sensitive = TRUE WHERE slug = ?", store.AttributeSlugPrimaryChannel)
	requirements.NoError(err)
	for _, args := range []map[string]any{{"person_id": person.ID}, {"person_id": person.ID, "include_sensitive": true}, {"person_id": person.ID, "fields": []any{"values"}}} {
		private := callRecordTool[mcpserver.FactClaimSelections](t, session, "list_person_fact_claims", args)
		requirements.Len(private.Claims, 1)
		assertions.Nil(private.Claims[0].SubmittedValue)
		assertions.Nil(private.Claims[0].NormalizedValue)
		assertions.Equal(claims.Claims[0].ValueFingerprint, private.Claims[0].ValueFingerprint)
	}
	private := callRecordTool[mcpserver.FactClaimSelections](t, session, "list_person_fact_claims", map[string]any{"person_id": person.ID, "fields": []any{"values"}, "include_sensitive": true})
	requirements.Len(private.Claims, 1)
	assertions.Equal(claims.Claims[0].SubmittedValue, private.Claims[0].SubmittedValue)
}

func TestMCPFactMergeSnapshotsCandidateReviewAndNativeSplitETags(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	// The primary Store/API/SDK split is real. A test executable cannot run the
	// separate installed CLI cache worker; exercise its native stale-cache receipt.
	oldRefresh := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(context.Context, string) error { return errors.New("synthetic cache worker unavailable") }
	t.Cleanup(func() { runDerivedCacheSubprocess = oldRefresh })
	st, person, backend, _, _ := recordSDKFixture(t)
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyRecords, mcpserver.OperationFamilyMergeRecovery}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	participant, err := st.EnsureParticipant("absorbed@example.test", "Synthetic Absorbed Person", "example.test")
	requirements.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	for id, value := range map[int64]string{person.ID: "email", absorbed.ID: "chat"} {
		_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}, Source: store.ProvenanceUser})
		requirements.NoError(err)
	}
	_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: absorbed.ID, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Synthetic private absorbed Notes")}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	person, err = st.GetPersonContext(t.Context(), person.ID)
	requirements.NoError(err)
	absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
	requirements.NoError(err)
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: person.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: person.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-merge-operation", Actor: "user"})
	requirements.NoError(err)
	listing := callRecordTool[generated.PersonMergesResponse](t, session, "list_person_merges", map[string]any{"person_id": person.ID, "limit": 1, "offset": 0})
	requirements.Len(listing.Merges, 1)
	assertions.Equal(merged.Merge.ID, listing.Merges[0].Merge.ID)
	detail := callRecordTool[generated.PersonMergeDetail](t, session, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	requirements.NotEmpty(detail.ReviewCandidates)
	snapshot := callRecordTool[mcpserver.MergeSnapshotSelection](t, session, "get_person_merge_snapshot", map[string]any{"merge_id": merged.Merge.ID})
	assertions.True(snapshot.SourceVerified)
	assertions.Equal(merged.Merge.SnapshotSHA256, snapshot.SourceSHA256)
	requirements.NotNil(snapshot.Rows)
	data, err := json.Marshal(snapshot)
	requirements.NoError(err)
	assertions.NotContains(string(data), "Synthetic private absorbed Notes")
	for _, row := range *snapshot.Rows {
		assertions.Empty(row.Columns)
	}
	sensitive := callRecordTool[mcpserver.MergeSnapshotSelection](t, session, "get_person_merge_snapshot", map[string]any{"merge_id": merged.Merge.ID, "fields": []any{"persons", "rows", "columns"}, "include_sensitive": true})
	data, err = json.Marshal(sensitive)
	requirements.NoError(err)
	assertions.Contains(string(data), "Synthetic private absorbed Notes")
	current := callRecordTool[mcpserver.PersonRecord](t, session, "get_person_record", map[string]any{"person_id": person.ID, "fields": []any{}})
	count := approvals
	foreign, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "review_person_merge_candidate", Arguments: map[string]any{"merge_id": merged.Merge.ID, "candidate_id": detail.ReviewCandidates[0].ID + 10000, "person_id": person.ID, "etag": current.ETag, "decision": "reject"}})
	requirements.NoError(err)
	assertions.True(foreign.IsError)
	assertions.Equal(count, approvals, "candidate must belong to the disclosed merge")
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "split_person", Arguments: map[string]any{"person_id": person.ID, "merge_id": merged.Merge.ID, "participant_ids": person.ParticipantIDs, "etag": current.ETag, "idempotency_key": "synthetic-wrong-lineage"}})
	requirements.NoError(err)
	assertions.True(invalid.IsError)
	assertions.Contains(settingsMCPDiagnostic(invalid), "person_split_invalid_participants")
	historyBefore := callRecordTool[generated.PersonMergeDetail](t, session, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	assertions.Empty(historyBefore.Splits, "native participant ownership refusal rolls back")
	decision := callRecordTool[mcpserver.MergeCandidateDecision](t, session, "review_person_merge_candidate", map[string]any{"merge_id": merged.Merge.ID, "candidate_id": detail.ReviewCandidates[0].ID, "person_id": person.ID, "etag": current.ETag, "decision": "reject"})
	assertions.Equal("rejected", decision.Candidate.State)
	assertions.NotEqual(current.ETag, decision.ETag)
	count = approvals
	stale, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "split_person", Arguments: map[string]any{"person_id": person.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{participant}, "etag": current.ETag, "idempotency_key": "synthetic-split-operation"}})
	requirements.NoError(err)
	assertions.True(stale.IsError)
	assertions.Equal(count, approvals)
	split := callRecordTool[mcpserver.PersonSplitRecord](t, session, "split_person", map[string]any{"person_id": person.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{participant}, "etag": decision.ETag, "idempotency_key": "synthetic-split-operation"})
	requirements.NotEmpty(split.ETag)
	requirements.NotEmpty(split.NewPersonETag)
	assertions.True(split.Result.ExactReversal)
	assertions.Equal(person.ID, split.Result.SourcePerson.ID)
	assertions.NotEqual(person.ID, split.Result.NewPerson.ID)
	source := callRecordTool[mcpserver.PersonRecord](t, session, "get_person_record", map[string]any{"person_id": person.ID, "fields": []any{}})
	newPerson := callRecordTool[mcpserver.PersonRecord](t, session, "get_person_record", map[string]any{"person_id": split.Result.NewPerson.ID, "fields": []any{}})
	assertions.Equal(source.ETag, split.ETag)
	assertions.Equal(newPerson.ETag, split.NewPersonETag)
	history := callRecordTool[generated.PersonMergeDetail](t, session, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	requirements.Len(history.Splits, 1)
	assertions.Equal(split.Result.Split.ID, history.Splits[0].ID)
	assertions.Equal(generated.PersonSplitResultCacheStateStale, split.Result.CacheState)
	replayed := callRecordTool[mcpserver.PersonSplitRecord](t, session, "split_person", map[string]any{"person_id": person.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{participant}, "etag": decision.ETag, "idempotency_key": "synthetic-split-operation"})
	assertions.Equal(split.Result.Split.ID, replayed.Result.Split.ID)
	assertions.Equal(split.ETag, replayed.ETag)
	assertions.Equal(split.NewPersonETag, replayed.NewPersonETag)
	history = callRecordTool[generated.PersonMergeDetail](t, session, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	assertions.Len(history.Splits, 1, "identical retry must not create another person or split")
	conflict, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "split_person", Arguments: map[string]any{"person_id": person.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{}, "etag": decision.ETag, "idempotency_key": "synthetic-split-operation"}})
	requirements.NoError(err)
	assertions.True(conflict.IsError)
	assertions.Contains(settingsMCPDiagnostic(conflict), "person_split_idempotency_conflict")
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), split.Result.SourcePerson.ID, split.Result.SourcePerson.Revision, new("Synthetic changed after split"))
	requirements.NoError(err)
	replayed = callRecordTool[mcpserver.PersonSplitRecord](t, session, "split_person", map[string]any{"person_id": person.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{participant}, "etag": decision.ETag, "idempotency_key": "synthetic-split-operation"})
	assertions.Equal(split.Result.SourcePerson.Revision, replayed.Result.SourcePerson.Revision, "replay retains immutable original receipt after later edits")
	assertions.Equal(split.ETag, replayed.ETag)
	thirdParticipant, err := st.EnsureParticipant("later-survivor@example.test", "Synthetic Later Survivor", "example.test")
	requirements.NoError(err)
	third, _, err := st.CreatePersonFromParticipant(thirdParticipant)
	requirements.NoError(err)
	currentSource, err := st.GetPersonContext(t.Context(), person.ID)
	requirements.NoError(err)
	_, err = st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: third.ID, AbsorbedID: currentSource.ID, ExpectedSurvivorRevision: third.Revision, ExpectedAbsorbedRevision: currentSource.Revision, IdempotencyKey: "synthetic-later-merge", Actor: "user"})
	requirements.NoError(err)
	replayed = callRecordTool[mcpserver.PersonSplitRecord](t, session, "split_person", map[string]any{"person_id": person.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{participant}, "etag": decision.ETag, "idempotency_key": "synthetic-split-operation"})
	assertions.Equal(split.Result.Split.ID, replayed.Result.Split.ID, "historical source may be absorbed after the committed split")
	assertions.Equal(split.ETag, replayed.ETag)
}

func TestMCPFactDiscoveryRefusesMissingOwnerContractsAndDelegatedAccess(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t), config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	native, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for _, route := range mcpFactRoutes {
		checks := []struct {
			id, property, query string
		}{{id: route.id}}
		for _, property := range route.properties {
			checks = append(checks, struct{ id, property, query string }{id: route.id, property: property})
		}
		for _, query := range route.query {
			checks = append(checks, struct{ id, property, query string }{id: route.id, query: query})
		}
		if route.contextID != "" {
			checks = append(checks, struct{ id, property, query string }{id: route.contextID})
		}
		for _, check := range checks {
			changed := *native
			changed.Routes = slices.Clone(native.Routes)
			if check.property == "" && check.query == "" {
				changed.Routes = slices.DeleteFunc(changed.Routes, func(r apiprotocol.MCPRouteDescriptor) bool { return r.OperationID == check.id })
			} else {
				for i := range changed.Routes {
					if changed.Routes[i].OperationID == check.id {
						changed.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(changed.Routes[i].RequestProperties), func(p string) bool { return p == check.property })
						changed.Routes[i].QueryParameters = slices.DeleteFunc(slices.Clone(changed.Routes[i].QueryParameters), func(p string) bool { return p == check.query })
					}
				}
			}
			assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name, "%+v", check)
		}
	}
	delegated := *native
	delegated.Delegated = true
	for _, name := range factMCPToolNames {
		assertions.NotContains(newDaemonMCPOperations(backend.client, &delegated).capabilities(), name)
	}
	for _, args := range []map[string]any{{"person_id": 1, "limit": 201}, {"person_id": 1, "offset": -1}, {"person_id": 1, "target": "employment:system:employment:sha256:invalid"}, {"person_id": 1, "limit": 1, "env": "invented"}} {
		result, err := backend.ExecuteOperation(t.Context(), "list_person_fact_evidence", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
}

func TestMCPFactSplitNativeZeroParticipantPartialAndReviewedCases(t *testing.T) {
	oldRefresh := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(context.Context, string) error { return errors.New("synthetic cache worker unavailable") }
	t.Cleanup(func() { runDerivedCacheSubprocess = oldRefresh })
	for _, scenario := range []string{"zero-participants", "partial", "accepted-candidate"} {
		t.Run(scenario, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st, source, backend, _, _ := recordSDKFixture(t)
			approvals := 0
			session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyRecords, mcpserver.OperationFamilyMergeRecovery}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
				approvals++
				return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
			}})
			var absorbed *store.Person
			selection := []int64{}
			if scenario == "zero-participants" {
				account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Synthetic Book", CanCreate: new(true)}}})
				requirements.NoError(err)
				requirements.Len(books, 1)
				remote := store.CardDAVRemoteResource{Href: books[0].CanonicalURL + "synthetic.vcf", RemoteUID: "synthetic-zero-participant", RemoteETag: `"one"`, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-zero-participant\r\nFN:Synthetic Zero Participant\r\nEND:VCARD\r\n"), SemanticHash: "synthetic-zero-hash", DisplayName: "Synthetic Zero Participant"}
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
				requirements.NoError(err)
				resource, err := st.GetCardDAVResourceContext(t.Context(), books[0].ID, remote.Href)
				requirements.NoError(err)
				requirements.NotNil(resource.PersonID)
				absorbed, err = st.GetPersonContext(t.Context(), *resource.PersonID)
				requirements.NoError(err)
				assertions.Empty(absorbed.ParticipantIDs)
			} else {
				first, err := st.EnsureParticipant("split-first@example.test", "Synthetic First", "example.test")
				requirements.NoError(err)
				selection = []int64{first}
				if scenario == "partial" {
					second, err := st.EnsureParticipant("split-second@example.test", "Synthetic Second", "example.test")
					requirements.NoError(err)
					_, err = st.LinkParticipants(first, second)
					requirements.NoError(err)
				}
				absorbed, _, err = st.CreatePersonFromParticipant(first)
				requirements.NoError(err)
			}
			if scenario == "accepted-candidate" {
				for id, channel := range map[int64]string{source.ID: "email", absorbed.ID: "chat"} {
					_, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &channel}, Source: store.ProvenanceUser})
					requirements.NoError(err)
				}
			} else {
				_, err := st.AddPersonNameContext(t.Context(), absorbed.ID, store.PersonNameInput{NameKind: store.PersonNameFormatted, Formatted: new("Synthetic aggregate name"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				requirements.NoError(err)
			}
			source, err := st.GetPersonContext(t.Context(), source.ID)
			requirements.NoError(err)
			absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
			requirements.NoError(err)
			merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: source.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: source.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-case-merge", Actor: "user"})
			requirements.NoError(err)
			current := callRecordTool[mcpserver.PersonRecord](t, session, "get_person_record", map[string]any{"person_id": source.ID, "fields": []any{}})
			if scenario == "accepted-candidate" {
				requirements.Len(merged.ReviewCandidates, 1)
				accepted := callRecordTool[mcpserver.MergeCandidateDecision](t, session, "review_person_merge_candidate", map[string]any{"person_id": source.ID, "merge_id": merged.Merge.ID, "candidate_id": merged.ReviewCandidates[0].ID, "etag": current.ETag, "decision": "accept"})
				assertions.Equal("accepted", accepted.Candidate.State)
				called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "split_person", Arguments: map[string]any{"person_id": source.ID, "merge_id": merged.Merge.ID, "participant_ids": selection, "etag": accepted.ETag, "idempotency_key": "synthetic-case-split"}})
				requirements.NoError(err)
				assertions.True(called.IsError)
				assertions.Contains(settingsMCPDiagnostic(called), "person_split_reviewed_candidates")
				detail := callRecordTool[generated.PersonMergeDetail](t, session, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
				assertions.Empty(detail.Splits)
				return
			}
			split := callRecordTool[mcpserver.PersonSplitRecord](t, session, "split_person", map[string]any{"person_id": source.ID, "merge_id": merged.Merge.ID, "participant_ids": selection, "etag": current.ETag, "idempotency_key": "synthetic-case-split"})
			assertions.Equal(scenario == "zero-participants", split.Result.ExactReversal)
			assertions.ElementsMatch(selection, split.Result.NewPerson.ParticipantIds)
			if scenario == "partial" {
				assertions.NotEmpty(split.Result.AmbiguousRows)
				assertions.Equal("retired_uid_alias_unchanged", split.Result.UIDAliasDisposition)
			}
		})
	}
}
