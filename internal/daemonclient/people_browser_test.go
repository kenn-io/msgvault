package daemonclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const peopleRelationshipTimezoneUTCPlus14 = "Pacific/Kiritimati"

func newPeopleBrowserTestEngine(t *testing.T, handler http.Handler) *PeopleBrowser {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	engine, err := NewEngine(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	return NewPeopleBrowser(engine)
}

func writePeopleBrowserJSON(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err := w.Write([]byte(body))
	assert.NoError(t, err)
}

func decodePeopleBrowserBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
		return nil
	}
	return body
}

func TestPeopleBrowserSearchForwardsCursorAndMapsPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/participants/search", r.URL.Path)
		body := decodePeopleBrowserBody(t, r)
		assert.Equal("alice", body["identity_query"])
		assert.Equal("people-cursor", body["cursor"])
		assert.InDelta(float64(25), body["limit"], 0)
		assert.Equal(map[string]any{
			"direction": "desc",
			"field":     "activity_count",
		}, body["sort"])
		writePeopleBrowserJSON(t, w, http.StatusOK, `{
			"rows":[{
				"id":11,"display_label":"Alice Example","display_name":"Alice",
				"partial_label":false,"activity_count":9,"meeting_count":3,"file_count":4,
				"current_relationship_temperature":62,"peak_relationship_temperature":97,"peak_relationship_year":2018,
				"identifiers":[{"type":"email","value":"alice@example.test","display_value":"alice@example.test","is_primary":true,"provenance":"archive","participant_id":11}],
				"source_counts":[{"source_type":"email","count":9}],
				"first_at":"2026-08-01T10:00:00Z","last_at":"2026-08-20T12:30:00Z",
				"cache_revision":"cache-7",
				"cluster":{"canonical_id":11,"member_ids":[11,14],"edges":[{"participant_a":11,"participant_b":14}]},
				"profile":{"id":51,"display_name":"Alice Curated","revision":3}
			}],
			"total_count":31,"cache_revision":"cache-7","next_cursor":"people-next","search_provenance":{}
		}`)
	}))

	page, err := engine.Search(t.Context(), peoplebrowser.SearchRequest{
		Query: "alice", Cursor: "people-cursor", Limit: 25,
	})
	require.NoError(err)
	require.Len(page.Rows, 1)
	assert.Equal(int64(31), page.TotalCount)
	assert.Equal("people-next", page.NextCursor)
	assert.Equal("cache-7", page.CacheRevision)
	assert.Equal(int64(11), page.Rows[0].ID)
	assert.Equal("Alice Example", page.Rows[0].DisplayLabel)
	assert.Equal(int64(3), page.Rows[0].MeetingCount)
	assert.Equal(62, page.Rows[0].CurrentRelationshipTemperature)
	assert.Equal(97, page.Rows[0].PeakRelationshipTemperature)
	assert.Equal(2018, page.Rows[0].PeakRelationshipYear)
	assert.Equal(int64(11), page.Rows[0].Identifiers[0].ParticipantID)
	require.NotNil(page.Rows[0].Cluster)
	assert.Equal([]int64{11, 14}, page.Rows[0].Cluster.MemberIDs)
	require.NotNil(page.Rows[0].Profile)
	assert.Equal(int64(51), page.Rows[0].Profile.ID)
}

func TestPeopleBrowserRelationshipCalendarForwardsScopeAndMapsAllFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/relationships/42/calendar", r.URL.Path)
		body := decodePeopleBrowserBody(t, r)
		assert.InDelta(float64(2026), body["year"], 0)
		assert.Equal("America/New_York", body["timezone"])
		writePeopleBrowserJSON(t, w, http.StatusOK, `{
			"participant_id":42,"canonical_id":7,"year":2026,"timezone":"America/New_York",
			"days":[{"date":"2026-01-02","sent":2,"received":3,"email":1,"chat":3,"meetings":1,"total":6,"modality_mask":7,"level":"THIRD_QUARTILE"}],
			"current":{"temperature":62,"rank":5,"population":100,"raw_score":7.5,"signals":{"sent_signal":2.5,"received_volume":3,"meeting_signal":1,"modalities":3}},
			"annual":[{"year":2018,"temperature":97,"rank":2,"population":80,"raw_score":9.5,"signals":{"sent_signal":4,"received_volume":5,"meeting_signal":2,"modalities":3}}],
			"peak_temperature":97,"peak_year":2018,"scoring_timezone":"UTC","score_version":1,
			"effective_date":"2026-08-22","cache_revision":"cache-24","identity_revision":9
		}`)
	}))

	calendar, err := engine.RelationshipCalendar(t.Context(), peoplebrowser.CalendarRequest{
		ParticipantID: 42, Year: 2026, Timezone: "America/New_York",
	})
	require.NoError(err)
	assert.Equal(int64(7), calendar.CanonicalID)
	assert.Equal(2026, calendar.Year)
	assert.Equal("America/New_York", calendar.Timezone)
	require.Len(calendar.Days, 1)
	assert.Equal(query.RelationshipCalendarDay{
		Date: "2026-01-02", Sent: 2, Received: 3, Email: 1, Chat: 3,
		Meetings: 1, Total: 6, ModalityMask: 7,
		Level: "THIRD_QUARTILE",
	}, calendar.Days[0])
	assert.Equal(62, calendar.Current.Temperature)
	assert.Equal(int64(5), calendar.Current.Rank)
	assert.InDelta(7.5, calendar.Current.RawScore, 0)
	assert.InDelta(2.5, calendar.Current.Signals.SentSignal, 0)
	require.Len(calendar.Annual, 1)
	assert.Equal(2018, calendar.Annual[0].Year)
	assert.Equal(97, calendar.Annual[0].Temperature)
	assert.Equal(97, calendar.PeakTemperature)
	assert.Equal(2018, calendar.PeakYear)
	assert.Equal("UTC", calendar.ScoringTimezone)
	assert.Equal(1, calendar.ScoreVersion)
	assert.Equal("2026-08-22", calendar.EffectiveDate)
	assert.Equal("cache-24", calendar.CacheRevision)
	assert.Equal(int64(9), calendar.IdentityRevision)
}

func TestPeopleBrowserRelationshipCalendarYearValidationUsesTimezone(t *testing.T) {
	require := require.New(t)
	now := time.Date(2025, time.December, 31, 10, 30, 0, 0, time.UTC)
	require.NoError(validatePeopleRelationshipYear(2026, peopleRelationshipTimezoneUTCPlus14, now))
	require.Error(validatePeopleRelationshipYear(2027, peopleRelationshipTimezoneUTCPlus14, now))
	require.Error(validatePeopleRelationshipYear(2026, "Mars/Olympus_Mons", now))
}

func TestPeopleBrowserRelationshipCalendarPreservesEmptyCollectionsAndAPIErrors(t *testing.T) {
	t.Run("rejects out-of-range modality mask", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"participant_id":42,"canonical_id":42,"year":2026,"timezone":"UTC",
				"days":[{"date":"2026-01-02","sent":0,"received":0,"email":0,"chat":0,"meetings":0,"total":0,"modality_mask":256,"level":"NONE"}],
				"current":{"temperature":0,"rank":0,"population":0,"raw_score":0,"signals":{"sent_signal":0,"received_volume":0,"meeting_signal":0,"modalities":0}},
				"annual":[],"peak_temperature":0,"peak_year":0,"scoring_timezone":"UTC","score_version":1,
				"effective_date":"2026-08-22","cache_revision":"cache-24","identity_revision":9
			}`)
		}))

		calendar, err := engine.RelationshipCalendar(t.Context(), peoplebrowser.CalendarRequest{
			ParticipantID: 42, Year: 2026, Timezone: "UTC",
		})
		require.Error(err)
		assert.Nil(calendar)
		assert.Contains(err.Error(), "modality mask 256")
	})

	t.Run("empty success", func(t *testing.T) {
		assert := assert.New(t)
		engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"participant_id":42,"canonical_id":42,"year":2026,"timezone":"UTC",
				"days":null,"current":{"temperature":0,"rank":0,"population":0,"raw_score":0,"signals":{"sent_signal":0,"received_volume":0,"meeting_signal":0,"modalities":0}},
				"annual":null,"peak_temperature":0,"peak_year":0,"scoring_timezone":"UTC","score_version":1,
				"effective_date":"2026-08-22","cache_revision":"cache-24","identity_revision":9
			}`)
		}))
		calendar, err := engine.RelationshipCalendar(t.Context(), peoplebrowser.CalendarRequest{
			ParticipantID: 42, Year: 2026, Timezone: "UTC",
		})
		require.NoError(t, err)
		assert.NotNil(calendar.Days)
		assert.Empty(calendar.Days)
		assert.NotNil(calendar.Annual)
		assert.Empty(calendar.Annual)
	})

	t.Run("typed daemon error", func(t *testing.T) {
		engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writePeopleBrowserJSON(t, w, http.StatusServiceUnavailable,
				`{"error":"analytical_cache_unavailable","message":"rebuild","readiness":"stale_schema"}`)
		}))
		_, err := engine.RelationshipCalendar(t.Context(), peoplebrowser.CalendarRequest{
			ParticipantID: 42, Year: 2026, Timezone: "UTC",
		})
		require.Error(t, err)
		var coded interface{ APIErrorCode() string }
		require.ErrorAs(t, err, &coded)
		assert.Equal(t, "analytical_cache_unavailable", coded.APIErrorCode())
	})
}

func TestPeopleBrowserRelationshipCalendarValidatesBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	for _, request := range []peoplebrowser.CalendarRequest{
		{ParticipantID: 0, Year: 2026, Timezone: "UTC"},
		{ParticipantID: 42, Year: 1969, Timezone: "UTC"},
	} {
		_, err := engine.RelationshipCalendar(t.Context(), request)
		require.Error(t, err)
	}
	assert.Zero(t, calls.Load())
}

func TestPeopleBrowserCompleteUsesPrivateBodyAndMapsTypedRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/participants/completions", r.URL.Path)
		assert.Empty(r.URL.RawQuery)
		body := decodePeopleBrowserBody(t, r)
		assert.Equal("José +1 415", body["query"])
		assert.InDelta(float64(8), body["limit"], 0)
		writePeopleBrowserJSON(t, w, http.StatusOK, `{
			"rows":[
				{"participant_id":11,"display_label":"José Example","kind":"name","value":"José Example","source":"profile"},
				{"participant_id":11,"display_label":"José Example","kind":"phone","value":"+1 415 555 0100","source":"whatsapp"}
			],
			"cache_revision":"cache-8"
		}`)
	}))

	page, err := engine.Complete(t.Context(), peoplebrowser.CompletionRequest{
		Query: "José +1 415", Limit: 8,
	})
	require.NoError(err)
	assert.Equal("cache-8", page.CacheRevision)
	require.Len(page.Rows, 2)
	assert.Equal(int64(11), page.Rows[0].ParticipantID)
	assert.Equal(query.PeopleCompletionName, page.Rows[0].Kind)
	assert.Equal("José Example", page.Rows[0].Value)
	assert.Equal(query.PeopleCompletionPhone, page.Rows[1].Kind)
	assert.Equal("whatsapp", page.Rows[1].Source)
}

func TestPeopleBrowserProfileAttributesAndInboxMappings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	profileDisplayName := "Alice Profile"
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/people":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"people":[{
				"id":51,"participant_ids":[11,14],"vcard_uid":"person-51","display_name":"Alice Profile","revision":2,
				"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
			}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/participants/11":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"id":11,"display_label":"Alice Example","partial_label":false,
				"identifiers":[],"activity_count":9,"file_count":4,"source_counts":[],
				"first_at":"2026-08-01T10:00:00Z","last_at":"2026-08-20T12:30:00Z","cache_revision":"cache-7"
			}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/people":
			body := decodePeopleBrowserBody(t, r)
			assert.Equal(map[string]any{"participant_id": float64(11)}, body)
			writePeopleBrowserJSON(t, w, http.StatusCreated, `{
				"id":51,"participant_ids":[11,14],"vcard_uid":"person-51","revision":1,
				"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
			}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/people/51/attributes":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"person_id":51,"attributes":[{
					"definition":{
						"id":7,"universal_id":"field-7","object_type":"person","slug":"nickname","label":"Nickname",
						"value_type":"text","field_type":"text","cardinality":"single","ownership":"user",
						"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
					},
					"current":[{
						"id":70,"person_id":51,"definition_id":7,"definition_slug":"nickname","ordinal":0,
						"value":{"type":"text","text":"Al"},"source":"user",
						"active_from":"2026-08-20T12:30:00Z","created_at":"2026-08-20T12:30:00Z"
					}]
				}]
			}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/participants/11/inboxes":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"rows":[{
					"source_id":4,"source_type":"whatsapp","source_identifier":"Personal",
					"conversation_count":2,"received_count":8,"sent_count":5,"latest_at":"2026-08-20T12:30:00Z"
				}],"cache_revision":"cache-7","identity_revision":12
			}`)
		default:
			http.NotFound(w, r)
		}
	}))

	contact, err := engine.GetContact(t.Context(), 11)
	require.NoError(err)
	assert.Equal("Alice Example", contact.DisplayLabel)

	profiles, err := engine.ListProfiles(t.Context())
	require.NoError(err)
	require.Len(profiles, 1)
	assert.Equal(store.Person{
		ID: 51, VCardUID: "person-51", DisplayName: &profileDisplayName, Revision: 2,
		ParticipantIDs: []int64{11, 14},
		CreatedAt:      time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC),
	}, profiles[0])

	person, err := engine.Promote(t.Context(), 11)
	require.NoError(err)
	assert.Equal(int64(51), person.ID)
	assert.Equal([]int64{11, 14}, person.ParticipantIDs)

	attributes, err := engine.ListAttributes(t.Context(), 51)
	require.NoError(err)
	assert.Equal(int64(51), attributes.PersonID)
	require.Len(attributes.Groups, 1)
	assert.Equal("nickname", attributes.Groups[0].Definition.Slug)
	require.Len(attributes.Groups[0].Current, 1)
	assert.Equal("Al", *attributes.Groups[0].Current[0].Value.Text)

	inboxes, err := engine.ListInboxes(t.Context(), 11)
	require.NoError(err)
	require.Len(inboxes.Rows, 1)
	assert.Equal("Personal", inboxes.Rows[0].SourceIdentifier)
	assert.Equal(int64(12), inboxes.IdentityRevision)
}

func TestPeopleBrowserCreateFieldOmitsSlugAndSetAttributePreservesTypedValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	expectedValueID := int64(70)
	ordinal := int64(2)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/attribute-definitions":
			assert.Equal(http.MethodPost, r.Method)
			body := decodePeopleBrowserBody(t, r)
			assert.NotContains(body, "slug")
			assert.Equal("Last score", body["label"])
			assert.Equal("real", body["value_type"])
			assert.Equal("text", body["field_type"])
			assert.Equal("multi", body["cardinality"])
			writePeopleBrowserJSON(t, w, http.StatusCreated, `{
				"id":8,"universal_id":"field-8","object_type":"person","slug":"last_score","label":"Last score",
				"value_type":"real","field_type":"text","cardinality":"multi","ownership":"user",
				"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
			}`)
		case "/api/v1/people/51/attributes/last_score":
			assert.Equal(http.MethodPut, r.Method)
			body := decodePeopleBrowserBody(t, r)
			assert.InDelta(float64(70), body["expected_value_id"], 0)
			assert.InDelta(float64(2), body["ordinal"], 0)
			assert.Equal("user", body["source"])
			assert.Equal(map[string]any{
				"real": float64(9.5), "type": "real",
			}, body["value"])
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"dry_run":false,"value":{
					"id":71,"person_id":51,"definition_id":8,"definition_slug":"last_score","ordinal":2,
					"value":{"type":"real","real":9.5},"source":"user",
					"active_from":"2026-08-20T12:30:00Z","created_at":"2026-08-20T12:30:00Z"
				}
			}`)
		default:
			http.NotFound(w, r)
		}
	}))

	definition, err := engine.CreateField(t.Context(), peoplebrowser.NewField{
		Label: "Last score", Kind: peoplebrowser.FieldKindNumber,
		Cardinality: store.AttributeCardinalityMulti,
	})
	require.NoError(err)
	assert.Equal("last_score", definition.Slug)
	assert.Equal(store.AttributeValueReal, definition.ValueType)

	write, err := engine.SetAttribute(t.Context(), peoplebrowser.SetAttributeRequest{
		PersonID: 51, Slug: "last_score", Ordinal: &ordinal,
		ExpectedValueID: &expectedValueID,
		Value: store.AttributeValue{
			Type: store.AttributeValueReal, Real: func() *float64 { value := 9.5; return &value }(),
		},
	})
	require.NoError(err)
	require.NotNil(write.Value)
	assert.Equal(int64(71), write.Value.ID)
	assert.InDelta(9.5, *write.Value.Value.Real, 0)
}

func TestPeopleBrowserAppendNotePreservesTextActorAndMapsWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/people/7/notes/append", r.URL.Path)
		body := decodePeopleBrowserBody(t, r)
		assert.Equal(map[string]any{
			"actor":  "mcp",
			"source": "user",
			"text":   "First line\nSeñor 🌍",
		}, body)
		writePeopleBrowserJSON(t, w, http.StatusOK, `{
			"dry_run":false,
			"value":{
				"id":72,"person_id":7,"definition_id":9,"definition_slug":"notes","ordinal":0,
				"value":{"type":"text","text":"Existing\nFirst line\nSeñor 🌍"},"source":"user","actor":"mcp",
				"active_from":"2026-08-20T12:30:00Z","created_at":"2026-08-20T12:30:00Z"
			},
			"superseded":{
				"id":71,"person_id":7,"definition_id":9,"definition_slug":"notes","ordinal":0,
				"value":{"type":"text","text":"Existing"},"source":"user",
				"active_from":"2026-08-19T12:30:00Z","active_until":"2026-08-20T12:30:00Z",
				"created_at":"2026-08-19T12:30:00Z","superseded_at":"2026-08-20T12:30:00Z"
			}
		}`)
	}))

	write, err := engine.AppendNote(t.Context(), peoplebrowser.AppendNoteRequest{
		PersonID: 7, Text: "First line\nSeñor 🌍", Actor: "mcp",
	})
	require.NoError(err)
	require.NotNil(write.Value)
	assert.Equal(int64(72), write.Value.ID)
	assert.Equal("Existing\nFirst line\nSeñor 🌍", *write.Value.Value.Text)
	assert.Equal("mcp", *write.Value.Actor)
	require.NotNil(write.Superseded)
	assert.Equal(int64(71), write.Superseded.ID)
	assert.Equal("Existing", *write.Superseded.Value.Text)
}

func TestPeopleBrowserProfileWritesForwardExplicitProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requestCount := 0
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		body := decodePeopleBrowserBody(t, r)
		assert.Equal("enrichment", body["source"])
		assert.Equal("mcp", body["actor"])
		switch r.URL.Path {
		case "/api/v1/people/7/attributes/notes":
			assert.Equal(http.MethodPut, r.Method)
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"dry_run":false,"value":null}`)
		case "/api/v1/people/7/notes/append":
			assert.Equal(http.MethodPost, r.Method)
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"dry_run":false,"value":null}`)
		default:
			http.NotFound(w, r)
		}
	}))

	text := "Replacement"
	_, err := engine.SetAttribute(t.Context(), peoplebrowser.SetAttributeRequest{
		PersonID: 7,
		Slug:     store.AttributeSlugNotes,
		Value:    store.AttributeValue{Type: store.AttributeValueText, Text: &text},
		Source:   store.ProvenanceEnrichment,
		Actor:    "mcp",
	})
	require.NoError(err)
	_, err = engine.AppendNote(t.Context(), peoplebrowser.AppendNoteRequest{
		PersonID: 7,
		Text:     "Fragment",
		Source:   store.ProvenanceEnrichment,
		Actor:    "mcp",
	})
	require.NoError(err)
	assert.Equal(2, requestCount)
}

func TestPeopleBrowserSetAttributeMapsConflictAndRetainsCurrentValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPut, r.Method)
		assert.Equal("/api/v1/people/51/attributes/nickname", r.URL.Path)
		writePeopleBrowserJSON(t, w, http.StatusConflict, `{
			"error":"attribute_value_conflict","message":"Attribute value changed; reload and retry",
			"current_value_id":72,
			"current_value":{
				"id":72,"person_id":51,"definition_id":7,"definition_slug":"nickname","ordinal":0,
				"value":{"type":"text","text":"Server value"},"source":"user",
				"active_from":"2026-08-20T12:30:00Z","created_at":"2026-08-20T12:30:00Z"
			}
		}`)
	}))

	text := "Draft value"
	_, err := engine.SetAttribute(t.Context(), peoplebrowser.SetAttributeRequest{
		PersonID: 51, Slug: "nickname",
		Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text},
	})
	require.Error(err)
	require.ErrorIs(err, peoplebrowser.ErrStaleValue)
	var stale peoplebrowser.StaleValueError
	require.ErrorAs(err, &stale)
	assert.Equal(int64(72), stale.CurrentValueID)
	require.NotNil(stale.CurrentValue)
	assert.Equal("Server value", *stale.CurrentValue.Value.Text)
}

func TestPeopleBrowserSetAttributeRetriesBusyBeforeMappingConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = time.Millisecond
	t.Cleanup(func() { operationBusyRetryDelay = oldDelay })

	var hits atomic.Int64
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPut, r.Method)
		assert.Equal("/api/v1/people/51/attributes/nickname", r.URL.Path)
		if hits.Add(1) == 1 {
			writePeopleBrowserJSON(t, w, http.StatusServiceUnavailable, `{
				"error":"operation_in_progress","message":"a sync is updating the archive"
			}`)
			return
		}
		writePeopleBrowserJSON(t, w, http.StatusConflict, `{
			"error":"attribute_value_conflict","message":"Attribute value changed; reload and retry",
			"current_value_id":72,
			"current_value":{
				"id":72,"person_id":51,"definition_id":7,"definition_slug":"nickname","ordinal":0,
				"value":{"type":"text","text":"Server value"},"source":"user",
				"active_from":"2026-08-20T12:30:00Z","created_at":"2026-08-20T12:30:00Z"
			}
		}`)
	}))

	notified := make(chan string, 1)
	engine.engine.store.SetBusyNotifier(func(message string) { notified <- message })
	text := "Draft value"
	_, err := engine.SetAttribute(t.Context(), peoplebrowser.SetAttributeRequest{
		PersonID: 51, Slug: "nickname",
		Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text},
	})
	require.Error(err)
	assert.Equal(int64(2), hits.Load())
	require.ErrorIs(err, peoplebrowser.ErrStaleValue)
	var stale peoplebrowser.StaleValueError
	require.ErrorAs(err, &stale)
	assert.Equal(int64(72), stale.CurrentValueID)
	require.NotNil(stale.CurrentValue)
	assert.Equal("Server value", *stale.CurrentValue.Value.Text)
	select {
	case message := <-notified:
		assert.Equal("a sync is updating the archive", message)
	default:
		assert.Fail("busy notifier was not called")
	}
}

func TestPeopleBrowserTextRevisionMappingAndParticipantIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal([]string{"11", "14"}, r.URL.Query()["participant_id"])
		assert.Contains(r.URL.RawQuery, "participant_id=11&participant_id=14")
		switch r.URL.Path {
		case "/api/v1/text/conversations":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"conversations":[{"conversation_id":31,"title":"Project chat","source_type":"whatsapp","message_count":5,"participant_count":3,"last_message_at":"2026-08-20T12:30:00Z","last_preview":"See you"}],
				"count":1,"has_more":false,"limit":25,"offset":0,"cache_revision":"text-conversations-7"
			}`)
		case "/api/v1/text/conversations/31/messages":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"messages":[{"id":91,"source_id":4,"source_message_id":"m-91","conversation_id":31,"source_conversation_id":"c-31","subject":"","snippet":"See you","from_email":"alice@example.test","from_name":"Alice","to":[],"cc":[],"bcc":[],"sent_at":"2026-08-20T12:30:00Z","size_estimate":20,"has_attachments":false,"attachment_count":0,"labels":[],"message_type":"whatsapp"}],
				"count":1,"has_more":false,"limit":25,"offset":0,"cache_revision":"text-messages-7"
			}`)
		default:
			http.NotFound(w, r)
		}
	}))

	filter := query.TextFilter{ParticipantIDs: []int64{11, 14}}
	conversations, err := engine.ListConversations(t.Context(), filter)
	require.NoError(err)
	require.Len(conversations.Rows, 1)
	assert.Equal(int64(31), conversations.Rows[0].ConversationID)
	assert.Zero(conversations.NextOffset)
	assert.True(conversations.Complete)
	assert.Equal("text-conversations-7", conversations.CacheRevision)

	messages, err := engine.ListConversationMessages(t.Context(), 31, filter)
	require.NoError(err)
	require.Len(messages.Rows, 1)
	assert.Equal(int64(91), messages.Rows[0].ID)
	assert.Zero(messages.NextOffset)
	assert.True(messages.Complete)
	assert.Equal("text-messages-7", messages.CacheRevision)
	assert.Equal(2, requests)
}

func TestPeopleBrowserTextPagesPreserveHasMoreOffsetAndLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/text/conversations":
			assert.Equal("100", r.URL.Query().Get("offset"))
			assert.Equal("25", r.URL.Query().Get("limit"))
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"conversations":[{"conversation_id":131,"title":"Page two","source_type":"whatsapp","message_count":4,"participant_count":2,"last_message_at":"2026-08-20T12:30:00Z","last_preview":"Still here"}],
				"count":1,"has_more":true,"limit":25,"offset":100
			}`)
		case "/api/v1/text/conversations/131/messages":
			assert.Equal("125", r.URL.Query().Get("offset"))
			assert.Equal("25", r.URL.Query().Get("limit"))
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"messages":[{"id":191,"source_id":4,"source_message_id":"m-191","conversation_id":131,"source_conversation_id":"c-131","subject":"","snippet":"Still here","from_email":"alice@example.test","from_name":"Alice","to":[],"cc":[],"bcc":[],"sent_at":"2026-08-20T12:30:00Z","size_estimate":20,"has_attachments":false,"attachment_count":0,"labels":[],"message_type":"whatsapp"}],
				"count":1,"has_more":true,"limit":25,"offset":125
			}`)
		default:
			http.NotFound(w, r)
		}
	}))

	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{
		Pagination: query.Pagination{Limit: 25, Offset: 100},
	})
	require.NoError(err)
	require.Len(conversations.Rows, 1)
	assert.Equal(125, conversations.NextOffset)
	assert.False(conversations.Complete)

	messages, err := engine.ListConversationMessages(t.Context(), 131, query.TextFilter{
		Pagination: query.Pagination{Limit: 25, Offset: 125},
	})
	require.NoError(err)
	require.Len(messages.Rows, 1)
	assert.Equal(150, messages.NextOffset)
	assert.False(messages.Complete)
}

func TestPeopleBrowserGetContactMapsTypedParticipantNotFound(t *testing.T) {
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writePeopleBrowserJSON(t, w, http.StatusNotFound, `{
			"error":"participant_not_found","message":"Participant cluster not found"
		}`)
	}))

	_, err := engine.GetContact(t.Context(), 11)
	require.Error(t, err)
	assert.ErrorIs(t, err, peoplebrowser.ErrContactNotFound)
}

func TestPeopleBrowserMeetingsUseParticipantTimelineCursorAndMapRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/participants/11/timeline", r.URL.Path)
		body := decodePeopleBrowserBody(t, r)
		assert.Equal("meeting-cursor", body["cursor"])
		assert.InDelta(float64(20), body["limit"], 0)
		assert.Equal("table", body["presentation"])
		assert.Equal([]any{map[string]any{
			"dimension": "message_type", "values": []any{"meeting_transcript"},
		}}, body["filters"])
		writePeopleBrowserJSON(t, w, http.StatusOK, `{
			"rows":[{
				"key":"message:88","kind":"meeting","anchor_message_id":88,"conversation_id":44,
				"occurred_at":"2026-08-20T12:30:00Z","match":{},"source_id":6,"source_type":"meeting",
				"source_identifier":"Recorder","message_type":"meeting_transcript","conversation_type":"meeting",
				"title":"Weekly sync","preview":"Discussed plans","participant_ids":[11,14],"participant_labels":["Alice"],
				"matched_sender_identities":[],"matched_recipient_identities":[],"message_count":1,
				"has_attachments":true,"attachment_count":2,"attachment_size":200,"deleted_from_source":false
			}],
			"total_count":7,"cache_revision":"cache-8","next_cursor":"meeting-next","search_provenance":{}
		}`)
	}))

	page, err := engine.ListMeetings(t.Context(), peoplebrowser.ContactPageRequest{
		ParticipantID: 11, Cursor: "meeting-cursor", Limit: 20,
	})
	require.NoError(err)
	require.Len(page.Rows, 1)
	assert.Equal("meeting-next", page.NextCursor)
	assert.Equal("cache-8", page.CacheRevision)
	assert.Equal(int64(88), page.Rows[0].ID)
	assert.Equal(int64(44), page.Rows[0].ConversationID)
	assert.Equal("Weekly sync", page.Rows[0].Subject)
	assert.Equal("Discussed plans", page.Rows[0].Snippet)
	assert.Equal("meeting_transcript", page.Rows[0].MessageType)
	assert.Equal(2, page.Rows[0].AttachmentCount)
}

func TestPeopleBrowserFilesAndActivityForwardCursorsAndMapRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(http.MethodPost, r.Method)
		body := decodePeopleBrowserBody(t, r)
		switch r.URL.Path {
		case "/api/v1/participants/11/files/search":
			assert.Equal("files-cursor", body["cursor"])
			assert.Equal([]any{"from_person"}, body["directions"])
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"files":[{
					"id":80,"key":"file:80","entry_key":"message:88","message_id":88,"conversation_id":44,
					"occurred_at":"2026-08-20T12:30:00Z","source_id":6,"source_type":"meeting","source_identifier":"Recorder",
					"containing_title":"Weekly sync","filename":"notes.pdf","mime_type":"application/pdf","mime_family":"pdf",
					"size_bytes":2048,"participant_ids":[11],"participant_labels":["Alice"],"participant_domains":[],
					"content_state":"local_content","content_available":true,
					"person_provenance":{"participant_ids":[11],"roles":["from"],"directions":["from_person"]}
				}],
				"total_count":3,"cache_revision":"cache-8","next_cursor":"files-next","search_provenance":{}
			}`)
		case "/api/v1/participants/11/timeline":
			assert.Equal("activity-cursor", body["cursor"])
			assert.NotContains(body, "filters")
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"rows":[{
					"key":"message:89","kind":"conversation","anchor_message_id":89,"conversation_id":45,
					"occurred_at":"2026-08-20T12:30:00Z","match":{},"source_id":4,"source_type":"whatsapp",
					"source_identifier":"Personal","message_type":"whatsapp","conversation_type":"chat",
					"title":"Morning","preview":"Hello","participant_ids":[11],"participant_labels":["Alice"],
					"matched_sender_identities":["alice@example.test"],"matched_recipient_identities":[],"message_count":1,
					"has_attachments":false,"attachment_count":0,"attachment_size":0,"deleted_from_source":false
				}],
				"total_count":12,"cache_revision":"cache-8","next_cursor":"activity-next","search_provenance":{}
			}`)
		default:
			http.NotFound(w, r)
		}
	}))

	files, err := engine.ListFiles(t.Context(), peoplebrowser.ContactPageRequest{
		ParticipantID: 11, Cursor: "files-cursor", Limit: 20,
	})
	require.NoError(err)
	require.Len(files.Rows, 1)
	assert.Equal("notes.pdf", files.Rows[0].Filename)
	assert.Equal(query.FileMIMEPDF, files.Rows[0].MIMEFamily)
	assert.Equal("files-next", files.NextCursor)

	activity, err := engine.ListActivity(t.Context(), peoplebrowser.ActivityPageRequest{
		ParticipantID: 11, Cursor: "activity-cursor", Limit: 20,
	})
	require.NoError(err)
	require.Len(activity.Rows, 1)
	assert.Equal("Morning", activity.Rows[0].Title)
	assert.Equal([]int64{11}, activity.Rows[0].ParticipantIDs)
	assert.Equal("activity-next", activity.NextCursor)
	assert.Equal(2, requests)
}

func TestDaemonPeopleBrowserImplementsBackend(t *testing.T) {
	var backend peoplebrowser.Backend = (*PeopleBrowser)(nil)
	assert.Nil(t, backend)
}

func TestPeopleBrowserStaleValueErrorMatchesSentinel(t *testing.T) {
	err := peoplebrowser.StaleValueError{CurrentValueID: 72}
	assert.ErrorIs(t, err, peoplebrowser.ErrStaleValue)
}
