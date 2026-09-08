package daemonclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
)

// briefStructuredFixture is a stored person brief structure. The citation
// order the sweep's parser produced for it is the interaction's item, the
// second highlight's item, then the uncertainty's item: the first highlight
// and the follow-up both re-cite the interaction's item. briefSentencesFixture
// carries the evidence_ordinals the daemon computes from that same order.
const briefStructuredFixture = `{
	"last_meaningful_interaction":{"evidence_id":"evidence:aaa","summary":"they were preparing for a role change"},
	"highlights":[
		{"text":"they spent the weekend learning to cook","speaker":"person","evidence_ids":["evidence:aaa"],"observed_at":null,"confidence_basis_points":800},
		{"text":"the autumn trip is still on","speaker":"person","evidence_ids":["evidence:bbb"],"observed_at":"2026-08-28","confidence_basis_points":600}
	],
	"follow_ups":[{"question":"how the transition went","why":"they were mid-change","highlight_index":0,"evidence_ids":["evidence:aaa"]}],
	"appreciations":[],
	"uncertainties":[{"text":"the move was mentioned in June and may have changed","kind":"stale","evidence_ids":["evidence:ccc"]}],
	"possible_attributes":[]
}`

const briefEvidenceFixture = `[
	{"ordinal":0,"evidence_id":11,"evidence_key":"key-a","source_ref":"message:1","source_url":"","directness":"direct-self","event_time":"2026-08-29T17:00:00Z","evidence_supported":true},
	{"ordinal":1,"evidence_id":12,"evidence_key":"key-b","source_ref":"message:2","source_url":"","directness":"direct-other","event_time":"2026-08-28T09:00:00Z","evidence_supported":true},
	{"ordinal":2,"evidence_id":13,"evidence_key":"key-c","source_ref":"message:3","source_url":"","directness":"direct-self","event_time":"2026-06-04T11:00:00Z","evidence_supported":false}
]`

const briefSentencesFixture = `[
	{"kind":"last_interaction","index":0,"text":"Last time you talked (Aug 29, chat): they were preparing for a role change.","evidence_ordinals":[0]},
	{"kind":"highlight","index":0,"text":"They said they spent the weekend learning to cook.","evidence_ordinals":[0]},
	{"kind":"highlight","index":1,"text":"They said the autumn trip is still on.","evidence_ordinals":[1]},
	{"kind":"follow_up","index":0,"text":"You may want to ask how the transition went.","evidence_ordinals":[0]},
	{"kind":"uncertainty","index":0,"text":"Check before assuming: the move was mentioned in June and may have changed.","evidence_ordinals":[2]}
]`

func briefResponseFixture(structured, evidence string) string {
	return briefResponseFixtureWithSentences(briefSentencesFixture, structured, evidence)
}

func briefResponseFixtureWithSentences(sentences, structured, evidence string) string {
	return `{
		"version":2,"status":"current","generated_at":"2026-08-29T18:42:10Z",
		"rendered_text":"Last time you talked (Aug 29, chat): they were preparing for a role change. They said they spent the weekend learning to cook. Check before assuming: the move was mentioned in June and may have changed.",
		"renderer_policy":"person-brief-render-v1",
		"sentences":` + sentences + `,
		"structured":` + structured + `,
		"evidence":` + evidence + `,
		"boundary":{"from_event_time":"2026-06-01T00:00:00Z","through_event_time":"2026-08-29T18:00:00Z"},
		"dropped_item_count":1,
		"program_id":"msgvault-person-brief","program_version":"v1",
		"provider":"fixture-provider","model":"gpt-test",
		"rejected_at":null,"rejected_reason":"","superseded_at":null
	}`
}

func TestPeopleBrowserGetPersonBriefMapsItemsToTheirCitedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var paths []string
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		writePeopleBrowserJSON(t, w, http.StatusOK,
			briefResponseFixture(briefStructuredFixture, briefEvidenceFixture))
	}))

	brief, err := engine.GetPersonBrief(t.Context(), 51)
	require.NoError(err)
	require.NotNil(brief)
	assert.Equal([]string{"GET /api/v1/people/51/brief"}, paths)
	assert.Equal(2, brief.Version)
	assert.Equal("current", brief.Status)
	assert.Equal(time.Date(2026, 8, 29, 18, 42, 10, 0, time.UTC), brief.GeneratedAt.UTC())
	assert.Contains(brief.RenderedText, "Last time you talked")
	assert.Equal(1, brief.DroppedItemCount)
	assert.Nil(brief.RejectedAt)

	require.Len(brief.Sentences, 5)
	assert.Equal(peoplebrowser.PersonBriefSentence{
		Kind: peoplebrowser.PersonBriefLastInteraction, Index: 0,
		Text:             "Last time you talked (Aug 29, chat): they were preparing for a role change.",
		EvidenceOrdinals: []int{0},
	}, brief.Sentences[0], "the sentence carries the join key the daemon computed")
	assert.Equal(peoplebrowser.PersonBriefUncertainty, brief.Sentences[4].Kind)

	kinds := make([]peoplebrowser.PersonBriefItemKind, 0, len(brief.Items))
	for _, item := range brief.Items {
		kinds = append(kinds, item.Kind)
	}
	assert.Equal([]peoplebrowser.PersonBriefItemKind{
		peoplebrowser.PersonBriefLastInteraction,
		peoplebrowser.PersonBriefHighlight, peoplebrowser.PersonBriefHighlight,
		peoplebrowser.PersonBriefFollowUp,
		peoplebrowser.PersonBriefUncertainty,
	}, kinds)

	interaction := brief.Items[0]
	assert.Equal("they were preparing for a role change", interaction.Text)
	require.Len(interaction.Evidence, 1)
	assert.Equal(peoplebrowser.PersonBriefEvidence{
		Ordinal: 0, EvidenceID: 11, SourceRef: "message:1", Directness: "direct-self",
		EventTime: time.Date(2026, 8, 29, 17, 0, 0, 0, time.UTC), Supported: true,
	}, interaction.Evidence[0])

	firstHighlight := brief.Items[1]
	assert.Equal("person", firstHighlight.Speaker)
	require.Len(firstHighlight.Evidence, 1)
	assert.Equal(int64(11), firstHighlight.Evidence[0].EvidenceID,
		"a re-cited item keeps the ordinal the daemon assigned it")

	secondHighlight := brief.Items[2]
	assert.Equal(1, secondHighlight.Index)
	require.Len(secondHighlight.Evidence, 1)
	assert.Equal(int64(12), secondHighlight.Evidence[0].EvidenceID)

	followUp := brief.Items[3]
	assert.Equal("how the transition went", followUp.Text)
	assert.Equal("they were mid-change", followUp.Why)
	require.Len(followUp.Evidence, 1)
	assert.Equal(int64(11), followUp.Evidence[0].EvidenceID)

	uncertainty := brief.Items[4]
	assert.Equal("stale", uncertainty.Reason)
	require.Len(uncertainty.Evidence, 1)
	assert.Equal(int64(13), uncertainty.Evidence[0].EvidenceID)
	assert.False(uncertainty.Evidence[0].Supported,
		"an invalidated source stays visible and is marked unsupported")
}

func TestPeopleBrowserGetPersonBriefRetainsEvidenceWithoutCitationOrder(t *testing.T) {
	structured := `{
		"last_meaningful_interaction":null,
		"highlights":[
			{"text":"first","speaker":"person","evidence_ids":["evidence:aaa"],"observed_at":null,"confidence_basis_points":800},
			{"text":"second","speaker":"person","evidence_ids":["evidence:zzz"],"observed_at":null,"confidence_basis_points":800}
		],
		"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]
	}`
	evidence := `[{"ordinal":0,"evidence_id":11,"evidence_key":"key-a","source_ref":"message:1","source_url":"","directness":"direct-self","event_time":"2026-08-29T17:00:00Z","evidence_supported":true}]`

	for name, sentences := range map[string]string{
		"paragraph alignment failed": `[]`,
		// The daemon could not account for the stored pointers, so it emits
		// sentences with no ordinals rather than a guess.
		"empty ordinals": `[
			{"kind":"highlight","index":0,"text":"They said first.","evidence_ordinals":[]},
			{"kind":"highlight","index":1,"text":"They said second.","evidence_ordinals":[]}
		]`,
		// A daemon that predates the join key omits the field entirely.
		"field absent": `[
			{"kind":"highlight","index":0,"text":"They said first."},
			{"kind":"highlight","index":1,"text":"They said second."}
		]`,
		// An ordinal that names no stored pointer is skipped, never invented.
		"ordinal out of range": `[
			{"kind":"highlight","index":0,"text":"They said first.","evidence_ordinals":[7]},
			{"kind":"highlight","index":1,"text":"They said second.","evidence_ordinals":[9]}
		]`,
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writePeopleBrowserJSON(t, w, http.StatusOK,
					briefResponseFixtureWithSentences(sentences, structured, evidence))
			}))

			brief, err := engine.GetPersonBrief(t.Context(), 51)
			require.NoError(err)
			require.NotNil(brief)
			assert.Equal([]peoplebrowser.PersonBriefEvidence{{
				Ordinal: 0, EvidenceID: 11, SourceRef: "message:1", Directness: "direct-self",
				EventTime: time.Date(2026, 8, 29, 17, 0, 0, 0, time.UTC), Supported: true,
			}}, brief.Evidence, "the whole brief's citations remain available without a sentence mapping")
			require.Len(brief.Items, 2)
			for _, item := range brief.Items {
				assert.Empty(item.Evidence, "a mapping the daemon did not supply is never guessed")
			}
			assert.NotEmpty(brief.RenderedText, "the paragraph the owner read is still shown")
		})
	}
}

func TestPeopleBrowserGetPersonBriefTreatsAMissingVersionAsNoBrief(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePeopleBrowserJSON(t, w, http.StatusNotFound,
			`{"error":"person_brief_not_found","message":"Person brief not found"}`)
	}))

	brief, err := engine.GetPersonBrief(t.Context(), 51)
	require.NoError(err)
	assert.Nil(brief)

	_, err = engine.GetPersonBrief(t.Context(), 0)
	require.Error(err, "a non-positive ID never reaches the daemon")
}

func TestPeopleBrowserGetPersonBriefPropagatesOtherFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePeopleBrowserJSON(t, w, http.StatusServiceUnavailable,
			`{"error":"person_brief_unavailable","message":"brief store unavailable"}`)
	}))

	brief, err := engine.GetPersonBrief(t.Context(), 51)
	require.Error(err)
	assert.Nil(brief)
	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusServiceUnavailable, apiErr.Status)
}

func TestPeopleBrowserPersonBriefMutations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests []string
	var bodies []map[string]any
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/people/51/brief-enrollment":
			bodies = append(bodies, decodePeopleBrowserBody(t, r))
			writePeopleBrowserJSON(t, w, http.StatusOK,
				`{"person_id":51,"enrolled":true,"enabled_at":"2026-08-29T18:42:10Z","actor":"api"}`)
		case "/api/v1/people/51/brief/generate":
			writePeopleBrowserJSON(t, w, http.StatusOK,
				`{"run_id":"run-1","attempt_id":"attempt-1","brief_version":3,"brief_failure_class":""}`)
		case "/api/v1/people/51/brief/reject":
			bodies = append(bodies, decodePeopleBrowserBody(t, r))
			writePeopleBrowserJSON(t, w, http.StatusOK,
				briefResponseFixture(briefStructuredFixture, briefEvidenceFixture))
		default:
			http.NotFound(w, r)
		}
	}))

	enrollment, err := engine.SetPersonBriefEnrollment(t.Context(), 51, true, true)
	require.NoError(err)
	require.NotNil(enrollment)
	assert.True(enrollment.Enrolled)
	assert.Equal("api", enrollment.Actor)
	require.NotNil(enrollment.EnabledAt)

	run, err := engine.GeneratePersonBrief(t.Context(), 51)
	require.NoError(err)
	require.NotNil(run)
	assert.Equal(peoplebrowser.PersonBriefRun{
		RunID: "run-1", AttemptID: "attempt-1", BriefVersion: 3,
	}, *run)

	rejected, err := engine.RejectPersonBrief(t.Context(), 51, "wrong thread")
	require.NoError(err)
	require.NotNil(rejected)
	assert.Equal(2, rejected.Version)

	assert.Equal([]string{
		"PUT /api/v1/people/51/brief-enrollment",
		"POST /api/v1/people/51/brief/generate",
		"POST /api/v1/people/51/brief/reject",
	}, requests)
	require.Len(bodies, 2)
	assert.Equal(map[string]any{"enrolled": true, "track": true}, bodies[0])
	assert.Equal(map[string]any{"reason": "wrong thread"}, bodies[1])

	for _, err := range []error{
		mutationError(engine.SetPersonBriefEnrollment(t.Context(), 0, true, false)),
		mutationError(engine.GeneratePersonBrief(t.Context(), 0)),
		mutationError(engine.RejectPersonBrief(t.Context(), 0, "")),
	} {
		assert.Error(err, "a non-positive ID never reaches the daemon")
	}
}

// mutationError discards a typed mutation result so a table can assert only
// that validation refused the request.
func mutationError[T any](_ *T, err error) error { return err }

func TestPeopleBrowserProfileCarriesTheCurrentBrief(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	briefStatus := http.StatusOK
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/people/51":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{
				"id":51,"participant_ids":[11],"vcard_uid":"person-51","revision":1,
				"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
			}`)
		case "/api/v1/people/51/brief":
			if briefStatus != http.StatusOK {
				writePeopleBrowserJSON(t, w, briefStatus,
					`{"error":"person_brief_not_found","message":"Person brief not found"}`)
				return
			}
			writePeopleBrowserJSON(t, w, http.StatusOK,
				briefResponseFixture(briefStructuredFixture, briefEvidenceFixture))
		default:
			writePeopleBrowserJSON(t, w, http.StatusNotFound,
				`{"error":"not_found","message":"resource not found"}`)
		}
	}))

	profile, err := engine.GetPersonProfile(t.Context(), 51)
	require.NoError(err)
	require.NotNil(profile)
	require.NotNil(profile.Brief)
	assert.Equal(2, profile.Brief.Version)

	briefStatus = http.StatusNotFound
	profile, err = engine.GetPersonProfile(t.Context(), 51)
	require.NoError(err)
	require.NotNil(profile)
	assert.Nil(profile.Brief, "a person without a brief still has a profile")
}

func TestPeopleBrowserWithoutBriefsHidesTheBriefSurfacesAndForwardsTheRest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var paths []string
	full := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		writePeopleBrowserJSON(t, w, http.StatusOK, `{
			"person":{"id":51,"participant_id":11,"display_label":"Alice Contact"}
		}`)
	}))
	gated := NewPeopleBrowserWithoutBriefs(full)

	_, reader := gated.(peoplebrowser.PersonBriefReader)
	assert.False(reader, "a gated backend must not advertise a brief read")
	_, writer := gated.(peoplebrowser.PersonBriefWriter)
	assert.False(writer, "a gated backend must not advertise brief mutations")

	_, err := gated.GetContact(t.Context(), 11)
	require.NoError(err)
	assert.Equal([]string{"/api/v1/participants/11"}, paths,
		"every other People operation still reaches the daemon")
}

func TestDaemonPeopleBrowserImplementsBriefSurfaces(t *testing.T) {
	var backend peoplebrowser.Backend = &PeopleBrowser{}
	_, reader := backend.(peoplebrowser.PersonBriefReader)
	assert.True(t, reader)
	_, writer := backend.(peoplebrowser.PersonBriefWriter)
	assert.True(t, writer)
}
