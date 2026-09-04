package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedSavedViewStateRoundTripsCanonicalDefinition(t *testing.T) {
	want := `{
		"query":"invoice",
		"search_mode":"full_text",
		"filters":[{"field":"source_id","operator":"eq","values":["9007199254740993"]}],
		"grouping":["sender"],
		"presentation":"table",
		"sort":[{"field":"sent_at","direction":"desc"}],
		"columns":["sender","subject"],
		"inspector_pinned":true
	}`

	var state generated.SavedViewStateEnvelope
	require.NoError(t, json.Unmarshal([]byte(want), &state))
	got, err := json.Marshal(state)
	require.NoError(t, err)
	assert.JSONEq(t, want, string(got))
}

func TestGeneratedPersonMergeRequiredResponseExposesProfiles(t *testing.T) {
	requirements := require.New(t)
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(generated.PersonMergeRequiredError{
		ErrorData: "person_merge_required",
		Message:   "These identities belong to different profiles",
		Profiles: []generated.PersonMergeProfile{{
			Etag: `"person-7-rev-3"`,
			Person: generated.Person{
				ID: 7, Revision: 3, VcardUID: "person-7",
				ParticipantIds: []int64{11}, CreatedAt: now, UpdatedAt: now,
			},
		}},
	})
	requirements.NoError(err)

	var response generated.LinkIdentityParticipantsErrorResponse
	requirements.NoError(json.Unmarshal(payload, &response))
	conflict := response.LinkIdentityParticipants_ErrorResponse_AnyOf
	requirements.NotNil(conflict)
	requirements.True(conflict.IsA())
	requirements.Len(conflict.A.Profiles, 1)
	assert.Equal(t, int64(7), conflict.A.Profiles[0].Person.ID)
}

func TestGeneratedEmploymentSourceConstantsRemainAssignable(t *testing.T) {
	var source = generated.User
	assert.Equal(t, generated.EmploymentBodySource("user"), source)
}

func TestGeneratedPersonFileGalleryContract(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	request := generated.PersonFileSearchHTTPRequest{
		Directions: []generated.PersonFileSearchHTTPRequestDirections{
			generated.PersonFileSearchHTTPRequestDirectionsFromPerson,
			generated.PersonFileSearchHTTPRequestDirectionsGroup,
		},
		Predicate: generated.ExploreHTTPRequest{},
		Sort:      generated.FileSearchSort{Field: "occurred_at", Direction: "desc"},
	}
	requirements.NoError(request.Validate())

	provenance := generated.PersonFileProvenance{
		Directions:     []generated.PersonFileProvenanceDirections{generated.FromPerson, generated.Group},
		ParticipantIds: []int64{17, 42},
		Roles:          []generated.PersonFileProvenanceRoles{generated.From, generated.ConversationMember},
	}
	requirements.NoError(provenance.Validate())

	encoded, err := json.Marshal(struct {
		Request    generated.PersonFileSearchHTTPRequest `json:"request"`
		Provenance generated.PersonFileProvenance        `json:"provenance"`
	}{Request: request, Provenance: provenance})
	requirements.NoError(err)
	assertions.JSONEq(`{
		"request": {
			"directions": ["from_person", "group"],
			"predicate": {},
			"sort": {"field": "occurred_at", "direction": "desc"}
		},
		"provenance": {
			"directions": ["from_person", "group"],
			"participant_ids": [17, 42],
			"roles": ["from", "conversation_member"]
		}
	}`, string(encoded))
}

func TestGeneratedPatchPersonCanClearDisplayName(t *testing.T) {
	body := generated.PatchPersonBody{DisplayName: nil}
	require.NoError(t, body.Validate())
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"display_name":null}`, string(encoded))
}

func TestGeneratedPersonMergeRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/people/7/merge", r.URL.Path)
		assert.Equal(`"person-7-r3", "person-9-r2"`, r.Header.Get("If-Match"))
		assert.Equal("generated-client-merge", r.Header.Get("Idempotency-Key"))
		var body generated.MergePersonRequest
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			http.Error(w, "invalid merge request", http.StatusBadRequest)
			return
		}
		assert.Equal(int64(9), body.AbsorbedPersonID)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"person-7-r4"`)
		_, _ = w.Write([]byte(`{
			"person":{
				"id":7,"vcard_uid":"survivor-uid","revision":4,
				"participant_ids":[70,90],
				"created_at":"2026-08-19T00:00:00Z",
				"updated_at":"2026-08-19T00:01:00Z"
			},
			"merge":{
				"id":12,"survivor_person_id":7,"absorbed_person_id":9,
				"current_person_id":7,"survivor_vcard_uid":"survivor-uid",
				"absorbed_vcard_uid":"absorbed-uid",
				"survivor_revision_before":3,"absorbed_revision_before":2,
				"survivor_revision_after":4,"actor":"user",
				"snapshot_version":1,
				"snapshot_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"created_at":"2026-08-19T00:01:00Z"
			},
			"review_candidates":[{
				"id":21,"merge_id":12,"person_id":7,"definition_id":4,
				"survivor_value_id":31,"absorbed_value_id":32,"state":"pending",
				"created_at":"2026-08-19T00:01:00Z"
			}],
			"identity_revision":42,
			"cache_state":"ready"
		}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(err)

	response, err := client.MergePersonsWithResponse(
		context.Background(), &generated.MergePersonsRequestOptions{
			PathParams: &generated.MergePersonsPath{ID: 7},
			Header: &generated.MergePersonsHeaders{
				IfMatch:        `"person-7-r3", "person-9-r2"`,
				IdempotencyKey: "generated-client-merge",
			},
			Body: &generated.MergePersonsBody{AbsorbedPersonID: 9},
		},
	)
	require.NoError(err)
	require.NotNil(response.JSON200)
	assert.Equal(int64(7), response.JSON200.Person.ID)
	assert.Equal(int64(12), response.JSON200.Merge.ID)
	assert.Equal(int64(42), response.JSON200.IdentityRevision)
	assert.Equal(generated.PersonMergeResultCacheStateReady, response.JSON200.CacheState)
	require.Len(response.JSON200.ReviewCandidates, 1)
	assert.Equal(int64(21), response.JSON200.ReviewCandidates[0].ID)
	require.NotNil(response.Headers200)
	assert.Equal(`"person-7-r4"`, response.Headers200.ETag)
}

func TestGeneratedCardDAVConflictAndPublicationRoundTrips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			assert.Equal(http.MethodPost, r.Method)
			assert.Equal("/api/v1/carddav/conflicts/7/resolve", r.URL.Path)
			var body generated.CardDAVResolveRequest
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal(generated.KeepRemote, body.Choice)
			_, _ = w.Write([]byte(`{"id":7,"status":"resolved","resolution":"keep_remote"}`))
		case 2:
			assert.Equal(http.MethodPost, r.Method)
			assert.Equal("/api/v1/carddav/publications/11", r.URL.Path)
			_, _ = w.Write([]byte(`{"person_id":11,"state":"published","desired":true,"address_book":{"id":2,"name":"Personal"}}`))
		case 3:
			assert.Equal(http.MethodDelete, r.Method)
			assert.Equal("/api/v1/carddav/publications/11", r.URL.Path)
			_, _ = w.Write([]byte(`{"person_id":11,"state":"unpublished","desired":false,"address_book":{"id":2,"name":"Personal"}}`))
		default:
			assert.Fail("unexpected request", "request %d", requests)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(err)

	resolved, err := client.ResolveCardDAVConflictWithResponse(t.Context(), &generated.ResolveCardDAVConflictRequestOptions{
		PathParams: &generated.ResolveCardDAVConflictPath{ID: 7},
		Body: &generated.ResolveCardDAVConflictBody{
			Choice: generated.KeepRemote,
		},
	})
	require.NoError(err)
	require.NotNil(resolved.JSON200)
	assert.Equal(generated.CardDAVConflictResolutionResponseResolutionKeepRemote, resolved.JSON200.Resolution)

	published, err := client.PublishCardDAVPersonWithResponse(t.Context(), &generated.PublishCardDAVPersonRequestOptions{
		PathParams: &generated.PublishCardDAVPersonPath{PersonID: 11},
	})
	require.NoError(err)
	require.NotNil(published.JSON200)
	require.NotNil(published.JSON200.AddressBook)
	assert.Equal(int64(2), published.JSON200.AddressBook.ID)

	unpublished, err := client.UnpublishCardDAVPersonWithResponse(t.Context(), &generated.UnpublishCardDAVPersonRequestOptions{
		PathParams: &generated.UnpublishCardDAVPersonPath{PersonID: 11},
	})
	require.NoError(err)
	require.NotNil(unpublished.JSON200)
	assert.False(unpublished.JSON200.Desired)
	assert.Equal(3, requests)
}

func TestOperationGeneratedClientDecodesSafeStatusListAndDetail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	statusRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/operations/status":
			assert.Equal(http.MethodGet, r.Method)
			statusRequests++
			if statusRequests > 1 {
				_, _ = w.Write([]byte(`{"lanes":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"lanes":[
				{"kind":"person_enrichment","lane":"person_facts","configured":false,"history_availability":"unavailable","unavailable_code":"person_enrichment_history_unavailable","supported_actions":[]},
				{"kind":"visual_embedding","lane":"visual_attachments","configured":true,"history_availability":"unavailable","unavailable_code":"visual_embedding_history_unavailable","related_status":"getVisualAttachmentStatus","supported_actions":["visual_resume"]}
			]}`))
		case "/api/v1/operations/runs":
			assert.Equal(http.MethodGet, r.Method)
			if r.URL.Query().Get("kind") == "" {
				_, _ = w.Write([]byte(`{"runs":[],"unavailable_kinds":[]}`))
				return
			}
			assert.Equal("source_sync", r.URL.Query().Get("kind"))
			assert.Equal("messages", r.URL.Query().Get("lane"))
			assert.Equal("succeeded", r.URL.Query().Get("state"))
			assert.Equal("25", r.URL.Query().Get("limit"))
			assert.Equal("opaque-current-page", r.URL.Query().Get("cursor"))
			_, _ = w.Write([]byte(`{"runs":[
				{"id":"1.safe-source-run","kind":"source_sync","lane":"messages","state":"succeeded","trigger":"scheduled","started_at":"2026-08-29T10:00:00Z","finished_at":"2026-08-29T10:00:02Z","counters":[{"name":"processed","unit":"messages","value":4}]},
				{"id":"1.safe-carddav-run","kind":"carddav_sync","lane":"contacts","state":"failed","trigger":"manual","started_at":"2026-08-29T11:00:00Z","finished_at":"2026-08-29T11:00:01Z","counters":[{"name":"books","unit":"books","value":1}],"error":{"code":"authentication_failed","message":"CardDAV authentication failed."}}
			],"next_cursor":"opaque-next-page","unavailable_kinds":[{"kind":"message_embedding","lane":"messages","unavailable_code":"message_embedding_history_unavailable"}]}`))
		case "/api/v1/operations/runs/1.safe-source-run":
			assert.Equal(http.MethodGet, r.Method)
			_, _ = w.Write([]byte(`{"id":"1.safe-source-run","kind":"source_sync","lane":"messages","state":"succeeded","trigger":"scheduled","started_at":"2026-08-29T10:00:00Z","finished_at":"2026-08-29T10:00:02Z","counters":[]}`))
		default:
			assert.Fail("unexpected request", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(err)

	status, err := client.GetOperationStatusWithResponse(t.Context())
	require.NoError(err)
	require.NotNil(status.JSON200)
	require.Len(status.JSON200.Lanes, 2)
	assert.Equal("person_enrichment", string(status.JSON200.Lanes[0].Kind))
	assert.NotNil(status.JSON200.Lanes[0].SupportedActions)
	assert.Empty(status.JSON200.Lanes[0].SupportedActions)
	require.NotNil(status.JSON200.Lanes[1].RelatedStatus)
	assert.Equal(generated.GetVisualAttachmentStatus, *status.JSON200.Lanes[1].RelatedStatus)
	assert.Equal(generated.VisualResume, status.JSON200.Lanes[1].SupportedActions[0])

	kind := generated.ListOperationRunsQueryKindSourceSync
	lane := generated.ListOperationRunsQueryLaneMessages
	state := generated.ListOperationRunsQueryStateSucceeded
	limit := int64(25)
	cursor := "opaque-current-page"
	list, err := client.ListOperationRunsWithResponse(t.Context(), &generated.ListOperationRunsRequestOptions{Query: &generated.ListOperationRunsQuery{
		Kind: &kind, Lane: &lane, State: &state, Limit: &limit, Cursor: &cursor,
	}})
	require.NoError(err)
	require.NotNil(list.JSON200)
	require.Len(list.JSON200.Runs, 2)
	assert.Equal(time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC), list.JSON200.Runs[0].StartedAt)
	require.NotNil(list.JSON200.Runs[0].Trigger)
	assert.Equal("scheduled", string(*list.JSON200.Runs[0].Trigger))
	require.Len(list.JSON200.Runs[0].Counters, 1)
	assert.Equal(generated.Processed, list.JSON200.Runs[0].Counters[0].Name)
	require.NotNil(list.JSON200.Runs[1].ErrorData)
	assert.Equal(generated.OperationPublicErrorCodeAuthenticationFailed, list.JSON200.Runs[1].ErrorData.Code)
	assert.Equal("CardDAV authentication failed.", list.JSON200.Runs[1].ErrorData.Message)
	assert.Equal("opaque-next-page", *list.JSON200.NextCursor)
	require.Len(list.JSON200.UnavailableKinds, 1)
	assert.Equal("message_embedding", string(list.JSON200.UnavailableKinds[0].Kind))

	detail, err := client.GetOperationRunWithResponse(t.Context(), &generated.GetOperationRunRequestOptions{
		PathParams: &generated.GetOperationRunPath{ID: "1.safe-source-run"},
	})
	require.NoError(err)
	require.NotNil(detail.JSON200)
	assert.Equal("source_sync", string(detail.JSON200.Kind))
	assert.NotNil(detail.JSON200.Counters)
	assert.Empty(detail.JSON200.Counters)

	emptyStatus, err := client.GetOperationStatusWithResponse(t.Context())
	require.NoError(err)
	require.NotNil(emptyStatus.JSON200)
	assert.NotNil(emptyStatus.JSON200.Lanes)
	assert.Empty(emptyStatus.JSON200.Lanes)
	emptyList, err := client.ListOperationRunsWithResponse(t.Context(), &generated.ListOperationRunsRequestOptions{})
	require.NoError(err)
	require.NotNil(emptyList.JSON200)
	assert.NotNil(emptyList.JSON200.Runs)
	assert.Empty(emptyList.JSON200.Runs)
	assert.NotNil(emptyList.JSON200.UnavailableKinds)
	assert.Empty(emptyList.JSON200.UnavailableKinds)
	assert.Equal(5, requests)

	encoded, err := json.Marshal(struct {
		Status *generated.OperationStatusResponse `json:"status"`
		List   *generated.OperationRunsResponse   `json:"list"`
		Detail *generated.OperationRunDetail      `json:"detail"`
	}{Status: status.JSON200, List: list.JSON200, Detail: detail.JSON200})
	require.NoError(err)
	for _, forbidden := range []string{
		`"method"`, `"path"`, `"action"`, `"url"`, `"href"`, `"vcard"`, `"credentials"`,
		`"fingerprint"`, `"provider"`, `"account"`, `"message_id"`, `"person_id"`, `"source_id"`,
		`"raw_error"`, `"evidence"`, `"usage"`, `"cost"`, `"args"`,
	} {
		assert.NotContains(string(encoded), forbidden)
	}
}

func TestGeneratedCardDAVConflictListAndDetailDecodeSafeContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	createdAt := time.Date(2026, time.August, 28, 9, 10, 11, 0, time.UTC)
	updatedAt := time.Date(2026, time.August, 28, 10, 11, 12, 0, time.UTC)
	resolvedAt := time.Date(2026, time.August, 28, 11, 12, 13, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/carddav/conflicts":
			assert.Equal(http.MethodGet, r.Method)
			_, _ = w.Write([]byte(`{"conflicts":[{"id":7,"address_book":{"id":2,"name":"Personal"},"status":"unresolved","local_state":"deleted","remote_state":"present","allowed_resolutions":["keep_local","keep_remote"],"updated_at":"2026-08-28T10:11:12Z"}]}`))
		case "/api/v1/carddav/conflicts/8":
			assert.Equal(http.MethodGet, r.Method)
			_, _ = w.Write([]byte(`{"id":8,"address_book":{"id":3,"name":"Archive"},"status":"resolved","resolution":"keep_local","base":{"state":"unavailable","emails":[],"phones":[]},"local":{"state":"present","display_name":"Alice","emails":["alice@example.test"],"phones":["+12025550123"],"truncated":true},"remote":{"state":"deleted","emails":[],"phones":[]},"allowed_resolutions":[],"created_at":"2026-08-28T09:10:11Z","updated_at":"2026-08-28T10:11:12Z","resolved_at":"2026-08-28T11:12:13Z"}`))
		default:
			assert.Fail("unexpected request", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(err)

	list, err := client.ListCardDAVConflictsWithResponse(t.Context())
	require.NoError(err)
	require.NotNil(list.JSON200)
	require.NotNil(list.JSON200.Conflicts)
	require.Len(list.JSON200.Conflicts, 1)
	require.NoError(list.JSON200.Validate())
	item := list.JSON200.Conflicts[0]
	assert.Equal(int64(7), item.ID)
	assert.Equal(int64(2), item.AddressBook.ID)
	assert.Equal("Personal", item.AddressBook.Name)
	assert.Equal(generated.CardDAVConflictResponseStatusUnresolved, item.Status)
	assert.Equal(generated.CardDAVConflictResponseLocalStateDeleted, item.LocalState)
	assert.Equal(generated.CardDAVConflictResponseRemoteStatePresent, item.RemoteState)
	assert.Equal([]generated.CardDAVConflictResponseAllowedResolutions{
		generated.CardDAVConflictResponseAllowedResolutionsKeepLocal,
		generated.CardDAVConflictResponseAllowedResolutionsKeepRemote,
	}, item.AllowedResolutions)
	assert.Equal(updatedAt, item.UpdatedAt)

	detail, err := client.GetCardDAVConflictWithResponse(t.Context(), &generated.GetCardDAVConflictRequestOptions{
		PathParams: &generated.GetCardDAVConflictPath{ID: 8},
	})
	require.NoError(err)
	require.NotNil(detail.JSON200)
	assert.Equal(int64(8), detail.JSON200.ID)
	assert.Equal(int64(3), detail.JSON200.AddressBook.ID)
	assert.Equal("Archive", detail.JSON200.AddressBook.Name)
	assert.Equal(generated.CardDAVConflictDetailResponseStatusResolved, detail.JSON200.Status)
	require.NotNil(detail.JSON200.Resolution)
	assert.Equal(generated.CardDAVConflictDetailResponseResolutionKeepLocal, *detail.JSON200.Resolution)
	assert.Equal(generated.CardDAVContactSummaryResponseStateUnavailable, detail.JSON200.Base.State)
	assert.NotNil(detail.JSON200.Base.Emails)
	assert.NotNil(detail.JSON200.Base.Phones)
	assert.Equal(generated.CardDAVContactSummaryResponseStatePresent, detail.JSON200.Local.State)
	require.NotNil(detail.JSON200.Local.DisplayName)
	assert.Equal("Alice", *detail.JSON200.Local.DisplayName)
	assert.Equal([]string{"alice@example.test"}, detail.JSON200.Local.Emails)
	assert.Equal([]string{"+12025550123"}, detail.JSON200.Local.Phones)
	require.NotNil(detail.JSON200.Local.Truncated)
	assert.True(*detail.JSON200.Local.Truncated)
	assert.Equal(generated.CardDAVContactSummaryResponseStateDeleted, detail.JSON200.Remote.State)
	assert.NotNil(detail.JSON200.Remote.Emails)
	assert.NotNil(detail.JSON200.Remote.Phones)
	assert.NotNil(detail.JSON200.AllowedResolutions)
	assert.Empty(detail.JSON200.AllowedResolutions)
	assert.Equal(createdAt, detail.JSON200.CreatedAt)
	assert.Equal(updatedAt, detail.JSON200.UpdatedAt)
	require.NotNil(detail.JSON200.ResolvedAt)
	assert.Equal(resolvedAt, *detail.JSON200.ResolvedAt)
	require.NoError(detail.JSON200.Validate())
	encoded, err := json.Marshal(detail.JSON200)
	require.NoError(err)
	assert.Contains(string(encoded), `"allowed_resolutions":[]`)
}

func TestGeneratedCardDAVConflictDecodesTyped409(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/carddav/conflicts/7/resolve", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"carddav_conflict_stale","message":"CardDAV conflict changed; refresh before trying again"}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(err)

	response, err := client.ResolveCardDAVConflictWithResponse(t.Context(), &generated.ResolveCardDAVConflictRequestOptions{
		PathParams: &generated.ResolveCardDAVConflictPath{ID: 7},
		Body: &generated.ResolveCardDAVConflictBody{
			Choice: generated.KeepLocal,
		},
	})
	require.Error(err)
	require.NotNil(response)
	require.NotNil(response.JSON409)
	assert.Equal(t, "carddav_conflict_stale", response.JSON409.ErrorData)
}

func TestGeneratedPersonMergeSnapshotPreservesArbitraryJSON(t *testing.T) {
	want := `{
		"version":1,
		"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"snapshot":{"persons":[{"id":7}],"rows":{"person_names":[1,2]}}
	}`
	var snapshot generated.PersonMergeSnapshotResponse
	require.NoError(t, json.Unmarshal([]byte(want), &snapshot))
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.JSONEq(t, want, string(encoded))
}

func TestGeneratedDailyNotePersonIDsRequirePositiveValues(t *testing.T) {
	require.NoError(t, (generated.CreateDailyNoteEntryRequest{
		Body:      "note",
		PersonIds: []int64{1},
	}).Validate())
	require.Error(t, (generated.CreateDailyNoteEntryRequest{
		Body:      "note",
		PersonIds: []int64{0},
	}).Validate())
	require.Error(t, (generated.CreateDailyNoteEntryRequest{
		Body:      "note",
		PersonIds: []int64{-1},
	}).Validate())
}

func TestGeneratedSourceIdentitiesPreserveRequiredEmptyArrays(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	emptyCatalog := generated.SourceIdentitiesResponse{
		SourceID: 7, Account: "account@example.test", Identities: []generated.SourceIdentityResponse{},
	}
	encoded, err := json.Marshal(emptyCatalog)
	requirements.NoError(err)
	assertions.JSONEq(`{"source_id":7,"account":"account@example.test","identities":[]}`, string(encoded))

	identity := generated.SourceIdentityResponse{
		Identifier: "Alias@Example.test", Signals: []string{},
		ConfirmedAt: time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC),
	}
	encoded, err = json.Marshal(identity)
	requirements.NoError(err)
	assertions.JSONEq(
		`{"identifier":"Alias@Example.test","signals":[],"confirmed_at":"2026-08-03T07:00:00Z"}`,
		string(encoded),
	)
}

func TestCreateCommunicationServiceAcceptsIdempotentOKResponse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/communication-services", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id":42,
			"slug":"example-chat",
			"display_label":"Example Chat",
			"aliases":[],
			"scope_policy":"none",
			"normalization":"lower",
			"normalization_version":1,
			"is_system":false,
			"is_active":true,
			"created_at":"2026-08-09T00:00:00Z",
			"updated_at":"2026-08-09T00:00:00Z"
		}`))
	}))
	t.Cleanup(server.Close)
	c, err := New(server.URL)
	require.NoError(err)

	service, err := c.CreateCommunicationService(
		context.Background(), &generated.CreateCommunicationServiceRequestOptions{
			Body: &generated.CreateCommunicationServiceBody{
				Slug: "example-chat", DisplayLabel: "Example Chat",
				ScopePolicy:   generated.CreateCommunicationServiceRequestScopePolicyNone,
				Normalization: generated.CreateCommunicationServiceRequestNormalizationLower,
			},
		},
	)
	require.NoError(err)
	require.NotNil(service)
	assert.Equal("example-chat", service.Slug)
}

func TestGeneratedEnumNamesPreserveSavedViewCompatibilityAndQualifyExploration(t *testing.T) {
	assertions := assert.New(t)
	assertions.Equal(generated.Asc, generated.SavedViewSortDirection("asc"))
	assertions.Equal(generated.Desc, generated.SavedViewSortDirection("desc"))
	assertions.Equal(generated.IdentitySearchSortDirectionAsc, generated.IdentitySearchSortDirection("asc"))
	assertions.Equal(generated.IdentitySearchSortDirectionDesc, generated.IdentitySearchSortDirection("desc"))
	assertions.Equal(generated.Files, generated.SavedViewStateEnvelopePresentation("files"))
	assertions.Equal(generated.Table, generated.SavedViewStateEnvelopePresentation("table"))
	assertions.Equal(generated.Timeline, generated.SavedViewStateEnvelopePresentation("timeline"))
	assertions.Equal(generated.ExploreFilterDimensionAfter, generated.ExploreFilterDimension("after"))
	assertions.Equal(generated.ExploreFilterDimensionIdentity, generated.ExploreFilterDimension("identity"))
	assertions.Equal(generated.ExploreGroupSortDirectionAsc, generated.ExploreGroupSortDirection("asc"))
	assertions.Equal(generated.ExploreGroupDimensionSource, generated.ExploreGroupDimension("source"))
	assertions.Equal(generated.ExploreGroupsHTTPRequestSearchModeFullText, generated.ExploreGroupsHTTPRequestSearchMode("full_text"))
}

func TestGeneratedExploreGroupingValidatesExactlyOneDimension(t *testing.T) {
	requirements := require.New(t)
	valid := generated.ExploreGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource,
	}}
	requirements.NoError(valid.Validate())

	requirements.Error((generated.ExploreGroupsHTTPRequest{}).Validate(), "empty grouping")
	requirements.Error((generated.ExploreGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource, generated.ExploreGroupDimensionMonth,
	}}).Validate(), "multiple grouping dimensions")

	fileValid := generated.FileGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource,
	}}
	requirements.NoError(fileValid.Validate())
	requirements.Error((generated.FileGroupsHTTPRequest{}).Validate(), "empty file grouping")
	requirements.Error((generated.FileGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource, generated.ExploreGroupDimensionMonth,
	}}).Validate(), "multiple file grouping dimensions")
}

func TestGeneratedFileMetadataRequiresPresenceButAcceptsEmptyLegacyStrings(t *testing.T) {
	t.Run("metadata response", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var present generated.FileMetadataResponse
		requirements.NoError(json.Unmarshal([]byte(
			`{"content_state":"metadata_only","entry_key":"source:1:message:m1","filename":"","mime_type":""}`,
		), &present))
		requirements.NotNil(present.Filename)
		requirements.NotNil(present.MimeType)
		assertions.Empty(*present.Filename)
		assertions.Empty(*present.MimeType)
		requirements.NoError(present.Validate(), "present empty strings are legitimate legacy metadata")

		missingFilename := present
		missingFilename.Filename = nil
		requirements.Error(missingFilename.Validate(), "missing required filename")
		missingMIME := present
		missingMIME.MimeType = nil
		requirements.Error(missingMIME.Validate(), "missing required MIME type")
	})

	t.Run("search row", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var present generated.FileSearchRow
		requirements.NoError(json.Unmarshal([]byte(
			`{"containing_title":"item","content_state":"metadata_only","entry_key":"message:1","filename":"","key":"file:1","mime_family":"other","mime_type":"","occurred_at":"2026-07-19T12:00:00Z","source_identifier":"archive@example.com","source_type":"synthetic"}`,
		), &present))
		requirements.NotNil(present.Filename)
		requirements.NotNil(present.MimeType)
		assertions.Empty(*present.Filename)
		assertions.Empty(*present.MimeType)
		requirements.NoError(present.Validate(), "present empty strings are legitimate legacy metadata")

		missingFilename := present
		missingFilename.Filename = nil
		requirements.Error(missingFilename.Validate(), "missing required filename")
		missingMIME := present
		missingMIME.MimeType = nil
		requirements.Error(missingMIME.Validate(), "missing required MIME type")
	})

	t.Run("person search row", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var present generated.PersonFileSearchRow
		requirements.NoError(json.Unmarshal([]byte(
			`{"containing_title":"item","content_state":"metadata_only","entry_key":"message:1","filename":"","key":"file:1","mime_family":"other","mime_type":"","occurred_at":"2026-07-19T12:00:00Z","person_provenance":{"directions":["to_person"],"participant_ids":[1],"roles":["to"]},"source_identifier":"archive@example.com","source_type":"synthetic"}`,
		), &present))
		requirements.NotNil(present.Filename)
		requirements.NotNil(present.MimeType)
		assertions.Empty(*present.Filename)
		assertions.Empty(*present.MimeType)
		requirements.NoError(present.Validate(), "present empty strings are legitimate legacy metadata")

		missingFilename := present
		missingFilename.Filename = nil
		requirements.Error(missingFilename.Validate(), "missing required filename")
		missingMIME := present
		missingMIME.MimeType = nil
		requirements.Error(missingMIME.Validate(), "missing required MIME type")
	})
}

func TestGeneratedPersonFactEvidenceRequiresPresenceButAcceptsEmptyStrings(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const payload = `{
		"id":1,"evidence_key":"public:1","source_class":"public_web",
		"directness":"direct","authority":"first_party","source_ref":"",
		"source_url":"https://example.test/profile","subject_ref":"",
		"excerpt":"","content_sha256":"","source_version":"",
		"event_time":"2026-08-24T12:00:00Z","recorded_time":"2026-08-24T12:00:00Z",
		"identity_score":10,"supported":true,"created_at":"2026-08-24T12:00:00Z",
		"latest_status":{"id":2,"generation_id":3,"evidence_key":"public:1",
			"source_version":"","supported":true,"reason":"observed",
			"created_at":"2026-08-24T12:00:00Z"}
	}`
	var public generated.PersonFactEvidence
	requirements.NoError(json.Unmarshal([]byte(payload), &public))
	requirements.NotNil(public.SourceRef)
	requirements.NotNil(public.ContentSha256)
	requirements.NotNil(public.SourceVersion)
	requirements.NotNil(public.SubjectRef)
	requirements.NotNil(public.Excerpt)
	requirements.NotNil(public.LatestStatus)
	requirements.NotNil(public.LatestStatus.SourceVersion)
	assertions.Empty(*public.SourceRef)
	assertions.Empty(*public.ContentSha256)
	assertions.Empty(*public.SourceVersion)
	assertions.Empty(*public.LatestStatus.SourceVersion)
	assertions.Empty(*public.SubjectRef)
	assertions.Empty(*public.Excerpt)
	requirements.NoError(public.Validate())
	requirements.NoError(public.LatestStatus.Validate())

	archive := public
	archive.SourceClass = "archive"
	empty := ""
	archive.SourceURL = &empty
	requirements.NoError(archive.Validate(), "archive evidence may omit a source URL value")

	missing := public
	missing.SourceRef = nil
	requirements.Error(missing.Validate(), "required source_ref must still be present")
	missing = public
	missing.SubjectRef = nil
	requirements.Error(missing.Validate(), "required subject_ref must still be present")
	missing = public
	missing.Excerpt = nil
	requirements.Error(missing.Validate(), "required excerpt must still be present")
	missingStatus := *public.LatestStatus
	missingStatus.SourceVersion = nil
	requirements.Error(missingStatus.Validate(), "required status source_version must still be present")
}

// TestGeneratedChangesResponseAcceptsTheFeedsOrdinaryPages holds the
// content-change feed to the same "required means present, not non-empty"
// distinction the file-metadata models above are held to.
//
// A row of that feed omits every column it has nothing to say about: a live
// message carries no deletion timestamps, and a chat message carries no subject,
// snippet, or platform id. Declaring any of those required in the OpenAPI
// document makes this validator reject them — required here means non-nil AND
// non-empty — so the published client would refuse the server's ordinary
// successful responses. next_cursor is the other side of that rule: the server
// publishes one on every page, empty ones included, so it can be required.
func TestGeneratedChangesResponseAcceptsTheFeedsOrdinaryPages(t *testing.T) {
	const liveEmail = `{
		"messages":[{
			"id":918,
			"source_id":1,
			"source_message_id":"18f2c9d0a1b3",
			"conversation_id":44,
			"message_type":"email",
			"subject":"Q4 planning",
			"snippet":"Here's the draft for Q4...",
			"sent_at":"2026-03-01T10:00:00Z",
			"size_estimate":8412,
			"has_attachments":false,
			"attachment_count":0,
			"content_changed_at":"2026-07-26T10:00:00.731123Z"
		}],
		"count":1,
		"has_more":false,
		"next_cursor":"1.eyJ0IjoiMjAyNi0wNy0yNlQxMDowMDowMC43MzExMjNaIiwiaSI6OTE4fQ",
		"server_time":"2026-07-26T10:00:03.114500Z",
		"complete_through":"2026-07-26T10:00:03.114488Z"
	}`

	t.Run("live message omits every unset timestamp", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(liveEmail), &page))
		requirements.NoError(page.Validate(),
			"a message that was never deleted and has no platform timestamps is the "+
				"common case, not an error")
		requirements.Len(page.Messages, 1)
		row := page.Messages[0]
		assertions.Nil(row.ReceivedAt, "received_at")
		assertions.Nil(row.InternalDate, "internal_date")
		assertions.Nil(row.DeletedAt, "deleted_at")
		assertions.Nil(row.DeletedFromSourceAt, "deleted_from_source_at")
	})

	t.Run("chat message omits subject, snippet, and platform id", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(`{
			"messages":[{
				"id":7,
				"source_id":2,
				"conversation_id":9,
				"message_type":"imessage",
				"size_estimate":0,
				"has_attachments":false,
				"attachment_count":0,
				"content_changed_at":"2026-07-26T10:00:00.731123Z"
			}],
			"count":1,
			"has_more":false,
			"next_cursor":"1.eyJ0IjoiMjAyNi0wNy0yNlQxMDowMDowMC43MzExMjNaIiwiaSI6N30",
			"server_time":"2026-07-26T10:00:03.114500Z",
			"complete_through":"2026-07-26T10:00:03.114488Z"
		}`), &page))
		requirements.NoError(page.Validate(),
			"chat platforms carry no subject and the store COALESCEs a missing "+
				"platform id to the empty string")
		requirements.Len(page.Messages, 1)
		row := page.Messages[0]
		assertions.Nil(row.Subject, "subject")
		assertions.Nil(row.Snippet, "snippet")
		assertions.Nil(row.SourceMessageID, "source_message_id")
	})

	t.Run("empty archive page still carries a cursor", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		const emptyArchive = `{
			"messages":[],
			"count":0,
			"has_more":false,
			"next_cursor":"1.eyJ0IjoiMDAwMS0wMS0wMVQwMDowMDowMFoiLCJpIjowfQ",
			"server_time":"2026-07-26T10:00:03.114500Z",
			"complete_through":"2026-07-26T10:00:03.114488Z"
		}`
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(emptyArchive), &page))
		requirements.NoError(page.Validate(),
			"a first poll of an empty archive has no last row, but it still hands "+
				"back the position the caller is standing at")
		assertions.Empty(page.Messages, "messages")
		assertions.NotEmpty(page.NextCursor, "next_cursor")

		page.NextCursor = ""
		requirements.Error(page.Validate(),
			"next_cursor is what a consumer sends back; a page without one leaves it "+
				"nothing to hold its place with, so required must mean non-empty here")
	})

	t.Run("a missing watermark is still rejected", func(t *testing.T) {
		requirements := require.New(t)
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(liveEmail), &page))
		requirements.Len(page.Messages, 1)
		page.Messages[0].ContentChangedAt = time.Time{}
		requirements.Error(page.Validate(),
			"content_changed_at is the row's watermark: it is what the feed orders "+
				"by, so loosening the other fields must not loosen this one")
		page.Messages[0].ContentChangedAt = time.Date(
			2026, 7, 26, 10, 0, 0, 731123000, time.UTC)
		page.ServerTime = time.Time{}
		requirements.Error(page.Validate(),
			"server_time is always a database clock reading")
		page.ServerTime = time.Date(2026, 7, 26, 10, 0, 3, 114500000, time.UTC)
		page.CompleteThrough = nil
		requirements.NoError(page.Validate(),
			"a nil complete_through represents the valid no-bound state")
	})

	t.Run("timestamps are typed and no bound is nullable", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)

		var noBoundYet generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(`{
			"messages":[],
			"count":0,
			"has_more":false,
			"next_cursor":"1.eyJ0IjoiMDAwMS0wMS0wMVQwMDowMDowMFoiLCJpIjowfQ",
			"server_time":"2026-07-26T10:00:03.114500Z",
			"complete_through":null
		}`), &noBoundYet))
		requirements.NoError(noBoundYet.Validate(),
			"a server with no commit bound must still decode and validate")
		assertions.Nil(noBoundYet.CompleteThrough)
	})
}

// TestListChangedMessagesRoundTripsTheCursorVerbatim covers the half of the
// change feed's client contract the response-model test above cannot reach: that
// the generated client builds the request the server accepts, and that the
// cursor survives the trip out through the generated query parameters byte for
// byte.
//
// The cursor is opaque, so the client has no way to repair one it damaged: a
// token altered on the way out is not a cursor this server issued and comes back
// 400, and one silently dropped restarts the consumer at the beginning of the
// archive. Neither shows up in the response model — only in the query string. So
// this walks the loop a consumer walks, taking next_cursor from one page and
// sending it as the next request, and asserts on what the client actually put on
// the wire.
func TestListChangedMessagesRoundTripsTheCursorVerbatim(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const watermark = "2026-07-26T10:00:00.731123Z"
	// An opaque token shaped like the ones the server issues, carrying the
	// sub-second watermark above and the id below. It is NOT one this server
	// would accept — it names no archive, and today's server rejects a cursor
	// that does not — which costs this test nothing: what it exercises is the
	// generated client's query-parameter round trip, and what matters is that
	// this exact string comes back off the wire.
	const cursor = "1.eyJ0IjoiMjAyNi0wNy0yNlQxMDowMDowMC43MzExMjNaIiwiaSI6OTE4fQ"

	var gotMethod, gotPath string
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"messages":[{
				"id":918,
				"source_id":1,
				"conversation_id":44,
				"message_type":"email",
				"subject":"Q4 planning",
				"size_estimate":8412,
				"has_attachments":false,
				"attachment_count":0,
				"content_changed_at":%q
			}],
			"count":1,
			"has_more":false,
			"next_cursor":%q,
			"server_time":"2026-07-26T10:00:03.114500Z",
			"complete_through":"2026-07-26T10:00:03.114488Z"
		}`, watermark, cursor)
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	// First poll: a consumer with no cursor yet. Sending cursor= would be a
	// different request than omitting it, so the client must omit.
	limit := int64(100)
	first, err := c.ListChangedMessages(context.Background(), &generated.ListChangedMessagesRequestOptions{
		Query: &generated.ListChangedMessagesQuery{Limit: &limit},
	})
	require.NoError(err, "ListChangedMessages first poll")
	assert.Equal(http.MethodGet, gotMethod, "method")
	assert.Equal("/api/v1/messages/changes", gotPath, "path")
	assert.Equal("100", gotQuery.Get("limit"), "limit query")
	assert.NotContains(gotQuery, "cursor", "an absent cursor must not be sent as an empty one")

	require.NotNil(first, "first page")
	assert.Equal(cursor, first.NextCursor, "the cursor must decode exactly as published")
	require.Len(first.Messages, 1, "messages")
	wantWatermark, err := time.Parse(time.RFC3339Nano, watermark)
	require.NoError(err)
	assert.Equal(wantWatermark, first.Messages[0].ContentChangedAt, "the row's watermark")

	// Second poll: the response fed straight back, exactly as the docs tell a
	// consumer to do it.
	second, err := c.ListChangedMessages(context.Background(), &generated.ListChangedMessagesRequestOptions{
		Query: &generated.ListChangedMessagesQuery{
			Cursor: &first.NextCursor,
			Limit:  &limit,
		},
	})
	require.NoError(err, "ListChangedMessages second poll")
	assert.Equal(cursor, gotQuery.Get("cursor"),
		"the cursor reached the wire altered: a token this server did not issue is "+
			"rejected, so a consumer that sends it back can no longer resume at all")
	assert.Equal("100", gotQuery.Get("limit"), "limit query")
	require.NotNil(second, "second page")
}

// TestListChangedMessagesDecodesTheDocumentedErrors covers the half of the
// contract the success paths cannot: the responses a consumer has to act on
// differently from one another.
//
// The feed answers 400 for a cursor it cannot use, 401 without a usable key,
// 429 when the caller has outrun the rate limiter, 500 when the watermark query
// fails, and 503 where the configured store cannot serve the feed at all. Only
// the first is recoverable, and only by restarting the sync from the beginning
// of the archive, so a consumer has to tell it apart from the ones it should
// retry — which means reading the `error` code out of the body. A generated
// client that models only 200 and 500 hands back an opaque status and leaves
// every consumer to decode the body by hand, so the codes are pinned here
// against the client the repository actually ships.
//
// 429 matters most to this endpoint of all of them: a consumer of an
// invalidation feed polls, and polling is what the limiter exists to catch.
func TestListChangedMessagesDecodesTheDocumentedErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		code    string
		message string
		body    func(*generated.ListChangedMessagesResp) *generated.ErrorResponse
	}{
		{
			name:    "an unusable cursor",
			status:  http.StatusBadRequest,
			code:    "invalid_cursor",
			message: "cursor was issued for a different archive",
			body:    func(r *generated.ListChangedMessagesResp) *generated.ErrorResponse { return r.JSON400 },
		},
		{
			name:    "a missing or rejected key",
			status:  http.StatusUnauthorized,
			code:    "unauthorized",
			message: "API key required",
			body:    func(r *generated.ListChangedMessagesResp) *generated.ErrorResponse { return r.JSON401 },
		},
		{
			name:    "a caller that has outrun the rate limiter",
			status:  http.StatusTooManyRequests,
			code:    "rate_limit_exceeded",
			message: "Too many requests. Please slow down.",
			body:    func(r *generated.ListChangedMessagesResp) *generated.ErrorResponse { return r.JSON429 },
		},
		{
			name:    "a watermark query that failed",
			status:  http.StatusInternalServerError,
			code:    "internal_error",
			message: "Message change query failed",
			body:    func(r *generated.ListChangedMessagesResp) *generated.ErrorResponse { return r.JSON500 },
		},
		{
			name:    "a store that cannot serve the feed",
			status:  http.StatusServiceUnavailable,
			code:    "feature_unavailable",
			message: "The configured store cannot serve the message change feed",
			body:    func(r *generated.ListChangedMessagesResp) *generated.ErrorResponse { return r.JSON503 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":%q,"message":%q}`, tc.code, tc.message)
			}))
			t.Cleanup(server.Close)

			c, err := New(server.URL)
			require.NoError(err, "New")

			resp, err := c.ListChangedMessagesWithResponse(
				context.Background(), &generated.ListChangedMessagesRequestOptions{})
			require.Error(err, "an error status must be reported as an error")
			require.NotNil(resp, "the response is what carries the decoded body")
			assert.Equal(tc.status, resp.StatusCode, "status code")

			decoded := tc.body(resp)
			require.NotNilf(decoded, "the client must decode the %d body: without it a "+
				"consumer cannot tell a cursor it must abandon from a condition it should "+
				"retry", tc.status)
			assert.Equal(tc.code, decoded.ErrorData,
				"the error code is what a consumer branches on")
			require.NotNil(decoded.Message, "message")
			assert.Equal(tc.message, *decoded.Message, "message")
		})
	}
}

func TestGeneratedGetAttachmentContentReturnsBinaryBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte{0x00, 0xff, 0x7b, 0x22, 0x6e, 0x6f, 0x74, 0x2d, 0x6a, 0x73, 0x6f, 0x6e}
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method, "method")
		assert.Equal("/api/v1/attachments/"+hash+"/content", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	c, err := generated.NewDefaultClient(server.URL, runtime.WithHTTPClient(httpClientDoer{client: http.DefaultClient}))
	require.NoError(err, "NewDefaultClient")

	got, err := c.GetAttachmentContent(context.Background(), &generated.GetAttachmentContentRequestOptions{
		PathParams: &generated.GetAttachmentContentPath{Hash: hash},
	})
	require.NoError(err, "GetAttachmentContent")
	require.NotNil(got, "response")
	assert.Equal(content, *got, "content")
}

func TestGetAttachmentContentVerifiesRequestedHash(t *testing.T) {
	require := require.New(t)
	want := []byte("public client attachment")
	corrupt := bytes.Clone(want)
	corrupt[0] ^= 0xff
	hash := fmt.Sprintf("%x", sha256.Sum256(want))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(corrupt)
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err)
	options := &generated.GetAttachmentContentRequestOptions{
		PathParams: &generated.GetAttachmentContentPath{Hash: hash},
	}
	_, err = c.GetAttachmentContent(context.Background(), options)
	require.ErrorIs(err, contentverify.ErrMismatch)
	response, err := c.GetAttachmentContentWithResponse(context.Background(), options)
	require.ErrorIs(err, contentverify.ErrMismatch)
	require.NotNil(response)
	assert.Equal(t, corrupt, response.Body)
}

func TestNewCreatesTypedClient(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":3}`))
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL)
	require.NoError(
		err, "New")

	stats, err := client.GetStats(context.Background())
	require.NoError(
		err, "GetStats")

	require.NotNil(stats)
	assert.Equal(t, int64(3), stats.TotalMessages)
}

func TestRunQueryDecodesScalarCells(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/query", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["n","s","b"],"rows":[[1,"x",true]],"row_count":1}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	got, err := c.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})
	require.NoError(
		err, "RunQuery")

	assert.Equal([]string{"n", "s", "b"}, got.Columns, "columns")
	require.Len(got.Rows, 1, "rows")
	numberCell, ok := got.Rows[0][0].(float64)
	require.True(ok, "number cell type")
	assert.InDelta(1.0, numberCell, 0, "number cell")
	assert.Equal("x", got.Rows[0][1], "string cell")
	assert.Equal(true, got.Rows[0][2], "bool cell")
}

func TestGetMessageRendersLargeIDInPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/24489626", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":24489626,"subject":"Large ID"}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	resp, err := c.GetMessageWithResponse(context.Background(), &generated.GetMessageRequestOptions{
		PathParams: &generated.GetMessagePath{ID: 24489626},
	})
	require.NoError(err, "GetMessageWithResponse")
	require.NotNil(resp.JSON200, "JSON200")
	assert.Equal(int64(24489626), resp.JSON200.ID, "id")
}

func TestListMessagesRendersLargeQueryValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages", r.URL.Path, "path")
		assert.Equal("12345678", r.URL.Query().Get("page"), "page query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"page":12345678,"page_size":20,"total":0}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	page := int64(12345678)
	resp, err := c.ListMessagesWithResponse(context.Background(), &generated.ListMessagesRequestOptions{
		Query: &generated.ListMessagesQuery{Page: &page},
	})
	require.NoError(err, "ListMessagesWithResponse")
	require.NotNil(resp.JSON200, "JSON200")
	assert.Equal(int64(12345678), resp.JSON200.Page, "page")
}

func TestAddAccountAcceptsIdempotentOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/accounts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","message":"account already exists"}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	got, err := c.AddAccount(context.Background(), &generated.AddAccountRequestOptions{
		Body: &generated.AddAccountBody{
			Email:    "alice@example.com",
			Enabled:  true,
			Schedule: "0 2 * * *",
		},
	})
	require.NoError(
		err, "AddAccount")

	assert.Equal("ok", got.Status, "status")
	assert.Equal("account already exists", got.Message, "message")
}

func TestCreatePersonAcceptsIdempotentOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/people", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 7,
			"vcard_uid": "17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
			"revision": 2,
			"participant_ids": [42],
			"created_at": "2026-07-29T12:00:00Z",
			"updated_at": "2026-07-29T12:00:00Z"
		}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	got, err := c.CreatePerson(context.Background(), &generated.CreatePersonRequestOptions{
		Body: &generated.CreatePersonBody{ParticipantID: 42},
	})
	require.NoError(err, "CreatePerson")

	assert.Equal(int64(7), got.ID, "id")
	assert.Equal(int64(2), got.Revision, "revision")
}

func TestImportMeetingAcceptsIdempotentOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/import/meeting", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"status":"updated","source_id":3,"message_id":901,"source_message_id":"meeting:synthetic-meeting-42"}`,
		))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	got, err := c.ImportMeeting(context.Background(), &generated.ImportMeetingRequestOptions{
		Body: &generated.ImportMeetingBody{
			Source: generated.Source{
				Identifier:   "local-meetings",
				AccountEmail: "user@example.com",
			},
			Meeting: generated.Meeting{
				ExternalID: "synthetic-meeting-42",
				StartedAt:  "2026-07-23T18:00:00Z",
			},
		},
	})
	require.NoError(err, "ImportMeeting update")

	assert.Equal(generated.MeetingImportResponseStatusUpdated, got.Status, "status")
	assert.Equal(int64(901), got.MessageID, "message id")
}

func TestStageDeletionAcceptsDryRunOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/deletions", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		// Dry runs return 200, not 201.
		_, _ = w.Write([]byte(`{"dry_run":true,"message_count":3,"sample_gmail_ids":["gm-1","gm-2","gm-3"]}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	sender := "alice@example.com"
	dryRun := true
	got, err := c.StageDeletion(context.Background(), &generated.StageDeletionRequestOptions{
		Body: &generated.StageDeletionBody{
			Filter: &generated.StageDeletionFilter{Sender: &sender},
			DryRun: &dryRun,
		},
	})
	require.NoError(
		err, "StageDeletion dry run")

	assert.True(got.DryRun, "dry_run")
	assert.Equal(int64(3), got.MessageCount, "message_count")
	assert.Len(got.SampleGmailIds, 3, "sample ids")
}
