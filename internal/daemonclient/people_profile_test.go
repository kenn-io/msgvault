package daemonclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

func TestPeopleBrowserGetPersonProfileComposesPersonRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var organizationLookups, employmentQuery []string
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/v1/people/51":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"id":51,"participant_ids":[11],"vcard_uid":"person-51","display_name":"Alice Profile","revision":2,
				"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
			}`)
		case "/api/v1/people/51/tracking":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"person_id":51,"tracked":true,"tracked_at":"2026-08-21T09:00:00Z"}`)
		case "/api/v1/people/51/contact-state":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"person_id":51,"first_contact_at":"2025-01-02T03:04:05Z","first_contact_ref":"message:1",
				"last_contact_at":"2026-08-20T12:30:00Z","last_contact_ref":"message:9","last_contact_channel":"chat",
				"last_contact_source_id":4,"last_contact_owner":"them","last_inbound_at":"2026-08-20T12:30:00Z",
				"last_inbound_ref":"message:9","last_outbound_at":"2026-08-19T08:00:00Z","last_outbound_ref":"message:8",
				"interaction_count":42,"inferred_channel":"chat","cadence_due_at":"2026-09-19T08:00:00Z",
				"cadence_status":"ok","stale":false,"computed_at":"2026-08-20T13:00:00Z"
			}`)
		case "/api/v1/people/51/attributes":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"person_id":51,"attributes":[{
					"definition":{
						"id":7,"universal_id":"`+store.AttributeUniversalIDReligion+`","object_type":"person","slug":"religion","label":"Religion",
						"value_type":"text","field_type":"text","cardinality":"single","ownership":"system","is_sensitive":true,
						"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
					},
					"current":[]
				}]
			}`)
		case "/api/v1/people/51/employments":
			employmentQuery = append(employmentQuery, r.URL.RawQuery)
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"employments":[
					{"id":5,"person_id":51,"organization_id":9,"title":"Engineer","start_date":{"year":2024,"month":3},
					 "is_current":true,"is_primary":true,"source":"user","revision":1,
					 "created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"},
					{"id":6,"person_id":51,"organization_id":12,"role":"Advisor","is_current":true,"is_primary":false,
					 "source":"extraction","revision":1,"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"}
				],
				"projection":{"employment_id":5,"organization_id":9,"organization_name":"Example Org","title":"Engineer","vcard":{}}
			}`)
		case "/api/v1/organizations/12":
			organizationLookups = append(organizationLookups, r.URL.Path)
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"organization":{"id":12,"name":"Second Org","kind":"company","revision":1,
				 "created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"}
			}`)
		case "/api/v1/people/51/relationships":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"relationships":[{
				"relationship":{"id":3,"source_person_id":51,"target_person_id":52,"relationship_type_id":1,
				 "type_slug":"partner","forward_label":"partner","reverse_label":"partner","is_symmetric":true,
				 "start_date":{"year":2020},"status":"active","source":"user","vcard_identity":{},
				 "created_by":"cli","updated_by":"cli","revision":1,
				 "created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"},
				"direction":"source","counterpart_person_id":52,"counterpart_label":"partner",
				"counterpart_display_name":"Bob Profile","counterpart_vcard_uid":"person-52"
			}]}`)
		case "/api/v1/people/51/profile":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"person":{"id":51,"participant_ids":[11],"vcard_uid":"person-51","revision":2,
				 "created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"},
				"contact_points":[
					{"person_id":51,"address_kind":"email","original_value":"alice@example.com","normalized_value":"alice@example.com",
					 "normalization":"email","normalization_version":1,
					 "envelope":{"id":1,"ordinal":0,"source":"vcard_import","pref":1,"type_label":"work",
					  "created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"}},
					{"person_id":51,"address_kind":"phone","original_value":"+15555550100","normalized_value":"+15555550100",
					 "normalization":"e164","normalization_version":1,
					 "envelope":{"id":2,"ordinal":1,"source":"user","active_until":"2026-01-01T00:00:00Z",
					  "created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"}}
				],
				"dates":[{"person_id":51,"date_kind":"birthday","original_value":"--04-12","date":{"month":4,"day":12},
				 "envelope":{"id":3,"ordinal":0,"source":"user","created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"}}],
				"categories":[{"person_id":51,"original_value":"people","normalized_value":"people",
				 "envelope":{"id":4,"ordinal":0,"source":"user","created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"}}],
				"addresses":[],"media":[],"names":[]
			}`)
		default:
			http.NotFound(w, r)
		}
	}))

	profile, err := engine.GetPersonProfile(t.Context(), 51)
	require.NoError(err)
	require.NotNil(profile)
	assert.Equal(int64(51), profile.Person.ID)
	assert.Equal("Alice Profile", *profile.Person.DisplayName)
	require.NotNil(profile.Tracked)
	assert.True(*profile.Tracked)

	require.NotNil(profile.ContactState)
	assert.Equal(time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC), profile.ContactState.LastContactAt.UTC())
	assert.Equal(time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC), profile.ContactState.LastOutboundAt.UTC())
	assert.Equal(store.ChannelChat, profile.ContactState.InferredChannel)
	assert.Equal(store.ChannelChat, profile.ContactState.LastContactChannel)
	assert.Equal(int64(42), profile.ContactState.InteractionCount)
	assert.Equal("message:9", profile.ContactState.LastContactRef)
	assert.Equal("ok", profile.ContactState.CadenceStatus)

	require.Len(profile.Attributes, 1)
	assert.True(profile.Attributes[0].Definition.IsSensitive, "definitions keep their sensitivity flag for the caller to filter")

	assert.Equal([]string{"current_only=true"}, employmentQuery)
	assert.Equal([]string{"/api/v1/organizations/12"}, organizationLookups, "the projection names the primary organization; only the other one is fetched")
	require.Len(profile.Employments, 2)
	assert.Equal(peoplebrowser.PersonEmployment{
		EmploymentID: 5, OrganizationID: 9, OrganizationName: "Example Org", Title: "Engineer",
		StartDate: "2024-03", IsCurrent: true, IsPrimary: true, Source: store.ProvenanceUser,
	}, profile.Employments[0])
	assert.Equal(peoplebrowser.PersonEmployment{
		EmploymentID: 6, OrganizationID: 12, OrganizationName: "Second Org", Role: "Advisor",
		IsCurrent: true, Source: store.ProvenanceExtraction,
	}, profile.Employments[1])

	assert.Equal([]peoplebrowser.PersonRelationshipSummary{{
		RelationshipID: 3, TypeSlug: "partner", CounterpartLabel: "partner", CounterpartPersonID: 52,
		CounterpartDisplayName: "Bob Profile", Direction: "source", Status: "active", StartDate: "2020",
		Source: store.ProvenanceUser,
	}}, profile.Relationships)

	assert.Equal([]peoplebrowser.PersonContactPointSummary{{
		Kind: "email", Value: "alice@example.com", TypeLabel: "work", Preferred: true, Source: store.ProvenanceVCardImport,
	}}, profile.ContactPoints, "ended contact points are dropped")
	assert.Equal([]peoplebrowser.PersonDateSummary{{Kind: "birthday", Date: "--04-12", Source: store.ProvenanceUser}}, profile.Dates)
	assert.Equal([]string{"people"}, profile.Categories)
}

func TestPeopleBrowserGetPersonProfileDegradesAbsentSubResources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/people/51":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"id":51,"participant_ids":[11],"vcard_uid":"person-51","revision":1,
				"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
			}`)
		case "/api/v1/people/51/tracking":
			writePeopleBrowserJSON(t, w, http.StatusServiceUnavailable, `{"error":"unavailable","message":"tracking store unavailable"}`)
		case "/api/v1/people/51/contact-state":
			writePeopleBrowserJSON(t, w, http.StatusNotFound, `{"error":"not_found","message":"resource not found"}`)
		case "/api/v1/people/51/attributes":
			writePeopleBrowserJSON(t, w, http.StatusServiceUnavailable, `{"error":"unavailable","message":"attribute store unavailable"}`)
		case "/api/v1/people/51/employments":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"employments":[]}`)
		case "/api/v1/people/51/relationships":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"relationships":[]}`)
		case "/api/v1/people/51/profile":
			writePeopleBrowserJSON(t, w, http.StatusNotFound, `{"error":"person_profile_not_found","message":"Person profile not found"}`)
		default:
			http.NotFound(w, r)
		}
	}))

	profile, err := engine.GetPersonProfile(t.Context(), 51)
	require.NoError(err)
	assert.Nil(profile.Tracked)
	assert.Nil(profile.ContactState)
	// An unavailable attribute store degrades to an empty set like the other
	// sub-resources instead of failing the whole profile.
	assert.Empty(profile.Attributes)
	assert.Equal([]peoplebrowser.PersonEmployment{}, profile.Employments)
	assert.Equal([]peoplebrowser.PersonRelationshipSummary{}, profile.Relationships)
	assert.Nil(profile.ContactPoints)
	assert.Nil(profile.Dates)
	assert.Nil(profile.Categories)
}

func TestPeopleBrowserGetPersonProfilePropagatesPersonNotFound(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePeopleBrowserJSON(t, w, http.StatusNotFound, `{"error":"person_profile_not_found","message":"Person profile not found"}`)
	}))

	profile, err := engine.GetPersonProfile(t.Context(), 404)
	require.Error(err)
	assert.Nil(profile)
	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusNotFound, apiErr.Status)
	assert.Equal("person_profile_not_found", apiErr.APIErrorCode())

	_, err = engine.GetPersonProfile(t.Context(), 0)
	require.Error(err, "a non-positive ID never reaches the daemon")
}

func TestDaemonPeopleBrowserImplementsProfileReader(t *testing.T) {
	var backend peoplebrowser.Backend = &PeopleBrowser{}
	_, ok := backend.(peoplebrowser.ProfileReader)
	assert.True(t, ok)
}
