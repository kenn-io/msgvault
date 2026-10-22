package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type mediaSearchFixture struct {
	*recordingFixture

	documentReport       *docbankmedia.SearchReport
	documentRequests     []docbankmedia.SearchRequest
	documentStatus       int
	duringDocumentSearch func()
	report               docbankmedia.SearchReport
	requests             []docbankmedia.SearchRequest
	validated            int
	duringSearch         func()
	remoteStatus         int
	waitForCancel        bool
}

func TestMediaSearchStatus(t *testing.T) {
	for _, state := range []string{"configured", "no reader", "no client"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			server := f.server(false)
			server.documentSearchRateLimiter.Close()
			server.documentSearchRateLimiter = NewRateLimiter(1, 1)
			t.Cleanup(server.documentSearchRateLimiter.Close)
			require.True(server.documentSearchRateLimiter.Allow("127.0.0.1"))
			gate := &recordingOperationGate{allow: false}
			server.operationGate = gate
			switch state {
			case "no reader":
				server.messageRecordings = nil
			case "no client":
				server.messageRecordings.Client = nil
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/media/search/status", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			request.Header.Set("X-Api-Key", recordingsTestAPIKey)
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Zero(f.validated)
			assert.Empty(f.requests)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var status MediaSearchStatusResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
			assert.Equal(state == "configured", status.Configured)
			assert.Zero(f.docbank.requestCount())
			begin, _ := gate.counts()
			assert.Zero(begin)
			if state == "no reader" {
				server.operationGate = nil
				code, _, raw := searchMediaFor(t, server, "q=words")
				assert.Equal(http.StatusServiceUnavailable, code, raw)
				assert.Contains(raw, "media_search_unavailable")
				assert.Contains(raw, "Configure the Docbank integration to search transcripts")
				assert.Empty(f.requests)
			}
		})
	}
}

func newMediaSearchFixture(t *testing.T) *mediaSearchFixture {
	t.Helper()
	f := &mediaSearchFixture{recordingFixture: newRecordingFixture(t), report: docbankmedia.SearchReport{
		MediaSourceSelection: true, MediaSelections: []docbankmedia.SearchMediaSelection{}, RequestedMode: "lexical", ActualMode: "lexical", Coverage: docbankmedia.SearchCoverage{State: "complete"}, Results: []docbankmedia.SearchHit{},
	}}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/processing/source-fences/resolve":
			assert.Equal(t, http.MethodPost, r.Method)
			var request map[string]json.RawMessage
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
				return
			}
			assert.NotContains(t, request, "content_version_ids")
			_ = json.NewEncoder(w).Encode(struct {
				Fence docbankmedia.SearchFence `json:"fence"`
			}{docbankmedia.SearchFence{VaultUID: "00000000-0000-4000-8000-000000000010", ContentVersionIDs: []string{}}})
		case "/api/v1/search", "/api/v1/search/validate":
			var request docbankmedia.SearchRequest
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
				return
			}
			assert.Equal(t, "lexical", request.Mode)
			assert.True(t, request.ContentFirst)
			if r.URL.Path == "/api/v1/search/validate" {
				assert.Nil(t, request.Fence)
				f.validated++
				_, _ = w.Write([]byte(`{"valid":true}`))
				return
			}
			if len(request.MediaSources) == 0 {
				for _, id := range request.Fence.ContentVersionIDs {
					assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, id)
				}
				f.documentRequests = append(f.documentRequests, request)
				assert.Equal(t, suppliedTranscriptProfile, request.Profile)
				if f.duringDocumentSearch != nil {
					f.duringDocumentSearch()
				}
				if f.documentStatus != 0 {
					w.WriteHeader(f.documentStatus)
					return
				}
				_ = json.NewEncoder(w).Encode(f.documentReport)
				return
			}
			f.requests = append(f.requests, request)
			if f.waitForCancel {
				<-r.Context().Done()
				return
			}
			if f.remoteStatus != 0 {
				w.WriteHeader(f.remoteStatus)
				return
			}
			if f.duringSearch != nil {
				f.duringSearch()
			}
			_ = json.NewEncoder(w).Encode(f.report)
		default:
			f.docbank.server.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(t, err)
	f.client = client
	return f
}

func (f *mediaSearchFixture) match(t *testing.T, id string) recordingSeed {
	t.Helper()
	seed := f.retained(t, id)
	_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE beeper_media_deliveries SET profile = 'asr' WHERE processing_key = ?`), "delivery-"+id)
	require.NoError(t, err)
	f.report.Results = append(f.report.Results, docbankmedia.SearchHit{VaultUID: "vault", NodeID: 1, ContentVersionID: seed.contentVersionID,
		Rank: len(f.report.Results) + 1, Excerpt: "quarterly numbers", Evidence: []docbankmedia.SearchEvidence{{Kind: "rendition_segment", BuildID: "build", SegmentID: "segment", MediaSources: []docbankmedia.SearchMediaSource{{SourceID: seed.sourceID, SourceVersionID: "version", ContentVersionID: seed.contentVersionID}}}},
	})
	f.report.MediaSelections = append(f.report.MediaSelections, docbankmedia.SearchMediaSelection{SourceID: seed.sourceID, SourceVersionID: "version", ContentVersionID: seed.contentVersionID, Origin: "generated", Completeness: "complete"})
	f.report.Coverage.ScopedDocuments++
	f.report.Coverage.CompleteDocuments++
	return seed
}

func searchMediaFor(t *testing.T, server *Server, query string) (int, MediaSearchResponse, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/media/search?"+query, nil)
	r.Header.Set("X-Api-Key", recordingsTestAPIKey)
	w := httptest.NewRecorder()
	server.Router().ServeHTTP(w, r)
	var response MediaSearchResponse
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	}
	return w.Code, response, w.Body.String()
}

func TestMediaSearchOccurrencesAndTiming(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	seed := f.match(t, "first")
	f.sameAudio["second"] = "first"
	id := f.message(t, "second")
	f.audio(t, id, "second", "", "delivery-first", &store.BeeperMediaResult{VaultUID: "vault", DocbankSourceID: seed.sourceID, ContentVersionID: seed.contentVersionID})
	f.report.Results[0].Evidence[0].TimeSpan = &docbankmedia.MediaTimeSpan{StartMS: 1200, EndMS: 2800}
	sentAt := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE messages SET subject = ?, sent_at = ? WHERE id = ?`), "Quarterly review", sentAt, seed.messageID)
	require.NoError(err)
	status, response, raw := searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	require.Equal(http.StatusOK, status, raw)
	require.Len(response.Results, 2)
	assert.Equal(seed.messageID, response.Results[0].MessageID)
	assert.Equal("voice.wav", response.Results[0].Filename)
	assert.Equal("Quarterly review", response.Results[0].ContainingTitle)
	require.NotNil(response.Results[0].OccurredAt)
	assert.True(sentAt.Equal(*response.Results[0].OccurredAt), "occurred_at %s", response.Results[0].OccurredAt)
	assert.Equal(id, response.Results[1].MessageID)
	assert.Equal(int64(1200), *response.Results[1].StartMS)
	assert.False(response.Partial)
	require.Len(f.requests, 1)
	assert.Equal([]string{seed.contentVersionID}, f.requests[0].Fence.ContentVersionIDs)
	assert.Equal(0, f.docbank.requestCount())
	status, response, raw = searchMediaFor(t, f.server(true), "q=quarterly+numbers&limit=1")
	require.Equal(http.StatusOK, status, raw)
	assert.Len(response.Results, 1)
	assert.True(response.Truncated)
}

func TestMediaSearchEmptyAndCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	status, response, raw := searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	require.Equal(http.StatusOK, status, raw)
	assert.Empty(response.Results)
	assert.False(response.Partial)
	assert.Equal(1, f.validated)
	assert.Empty(f.requests)
	seed := f.match(t, "filename-only")
	f.report.Results = []docbankmedia.SearchHit{}
	f.report.Coverage.State = "unknown"
	status, response, raw = searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	require.Equal(http.StatusOK, status, raw)
	assert.Empty(response.Results)
	assert.Equal("unknown", response.Coverage.State)
	assert.True(response.Partial)
	assert.Equal([]string{seed.contentVersionID}, f.requests[0].Fence.ContentVersionIDs)
}

func TestMediaSearchCompletePopulation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	f.retained(t, "seed")
	cloneSearchOccurrences(t, f, "msgvault:seed", 1000)
	seed := f.match(t, "beyond-worker-window")
	f.report.Coverage = docbankmedia.SearchCoverage{State: "incomplete", ScopedDocuments: 1002, CompleteDocuments: 1}
	status, response, raw := searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	require.Equal(http.StatusOK, status, raw)
	require.Len(response.Results, 1)
	assert.Equal(seed.messageID, response.Results[0].MessageID)
	require.Len(f.requests, 1)
	assert.Len(f.requests[0].Fence.ContentVersionIDs, 1002)
}

func TestMediaSearchParametersAndAuthorization(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	for _, query := range []string{"q=words&mode=semantic", "q=words&mode=hybrid", "q=words&mode=invalid", "q=words&limit=101", "q=words&person_id=0", "q=words&participant_id=7&person_id=7", "q=words&direction=from_person", "q=words&person_id=7&direction=invalid", "q=words&person_id=7&direction=", "q=" + url.QueryEscape(strings.Repeat("x", 8193))} {
		status, _, raw := searchMediaFor(t, f.server(true), query)
		want := http.StatusBadRequest
		if strings.Contains(query, "mode=semantic") || strings.Contains(query, "mode=hybrid") {
			want = http.StatusServiceUnavailable
		}
		assert.Equal(want, status, raw)
		if strings.Contains(query, "participant_id=7&person_id=7") {
			assert.Contains(raw, "mutually exclusive")
		}
		assert.Empty(f.requests)
	}
	w := httptest.NewRecorder()
	f.server(true).Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/media/search?q=words", nil))
	assert.Equal(http.StatusUnauthorized, w.Code)
}

// cloneSearchOccurrences creates clones that share the seed's attachment, message, source, and processing key and differ only in occurrence_ref and content_version_id.
func cloneSearchOccurrences(t *testing.T, f *mediaSearchFixture, ref string, count int) {
	t.Helper()
	require := require.New(t)

	// Clone mapped identities to exercise the public fence ceiling without thousands of imports.
	transaction, err := f.f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = transaction.Rollback() })
	for i := range count {
		_, err := transaction.Exec(f.f.Store.Rebind(`INSERT INTO beeper_media_occurrences (
			destination_key, occurrence_ref, revision, source_type, source_identifier, source_conversation_id,
			source_message_id, source_attachment_id, source_part_key, source_sha256, byte_length,
			occurrence_json, request_filename, request_mime_type, retention_state, vault_uid,
			source_id, source_version_id, content_version_id, processing_key)
			SELECT destination_key, ?, revision, source_type, source_identifier, source_conversation_id,
			source_message_id, source_attachment_id, source_part_key, source_sha256, byte_length,
			occurrence_json, request_filename, request_mime_type, retention_state, vault_uid,
			source_id, source_version_id, ?, processing_key
			FROM beeper_media_occurrences WHERE occurrence_ref = ?`), fmt.Sprintf("msgvault:copy-%d", i), fmt.Sprintf("content-%d", i), ref)
		require.NoError(err)
	}
	require.NoError(transaction.Commit())
}

func TestMediaSearchOversizedPopulation(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	f.retained(t, "seed")
	cloneSearchOccurrences(t, f, "msgvault:seed", docbankmedia.MaxSearchVersions)
	status, _, raw := searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	assert.Equal(http.StatusBadRequest, status, raw)
	assert.Contains(raw, "media_search_scope_limit")
	assert.Empty(f.requests)
}

func TestMediaSearchParticipantScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	selected := f.match(t, "selected")
	aliased := f.match(t, "aliased")
	participant := f.f.EnsureParticipant("selected@example.com", "Selected Person", "example.com")
	alias := f.f.EnsureParticipant("alias@example.com", "Alias", "example.com")
	_, err := f.f.Store.LinkParticipants(participant, alias)
	require.NoError(err)
	for messageID, sender := range map[int64]int64{selected.messageID: participant, aliased.messageID: alias} {
		_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), sender, messageID)
		require.NoError(err)
	}
	fullReport := f.report
	f.duringSearch = func() {
		fence := f.requests[len(f.requests)-1].Fence.ContentVersionIDs
		f.report = fullReport
		f.report.Results = slices.DeleteFunc(slices.Clone(fullReport.Results), func(hit docbankmedia.SearchHit) bool {
			return !slices.Contains(fence, hit.ContentVersionID)
		})
		f.report.MediaSelections = slices.DeleteFunc(slices.Clone(fullReport.MediaSelections), func(selection docbankmedia.SearchMediaSelection) bool {
			return !slices.Contains(fence, selection.ContentVersionID)
		})
		f.report.Coverage.ScopedDocuments, f.report.Coverage.CompleteDocuments = len(f.report.Results), len(f.report.Results)
	}
	search := func(server *Server, parameters string) []int64 {
		t.Helper()
		status, response, raw := searchMediaFor(t, server, "q=quarterly+numbers&"+parameters)
		require.Equal(http.StatusOK, status, raw)
		var messageIDs []int64
		for _, result := range response.Results {
			messageIDs = append(messageIDs, result.MessageID)
		}
		return messageIDs
	}
	byParticipant := fmt.Sprintf("participant_id=%d", participant)

	assert.ElementsMatch([]int64{selected.messageID, aliased.messageID}, search(f.server(true), byParticipant), "live links widen the cluster")
	_, err = f.f.Store.UnlinkParticipants(participant, alias)
	require.NoError(err)
	assert.Equal([]int64{selected.messageID}, search(f.server(true), byParticipant), "unlinked alias leaves the cluster")
	assert.Equal([]string{selected.contentVersionID}, f.requests[len(f.requests)-1].Fence.ContentVersionIDs)

	analytical := f.server(true)
	clusters := fmt.Sprintf(`(%d::BIGINT, %d::BIGINT), (%d::BIGINT, %d::BIGINT)`, participant, participant, alias, participant)
	engine, _ := newExploreDuckDBFixtureWithClusters(t, exploreFixtureDefaultMessages, exploreFixtureDefaultRecipients, clusters, 3)
	analytical.SetAnalyticsEngine(engine, AnalyticsModeDuckDB)
	assert.ElementsMatch([]int64{selected.messageID, aliased.messageID}, search(analytical, byParticipant), "committed Explore clusters win over live links")

	_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE messages SET sender_id = NULL WHERE id = ?`), selected.messageID)
	require.NoError(err)
	_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'mention')`), selected.messageID, participant)
	require.NoError(err)
	assert.Equal([]int64{selected.messageID}, search(f.server(true), byParticipant), "a mention matches like Explore's Person filter")
	assert.Empty(search(f.server(true), byParticipant+"&direction=from_person"), "a direction disables membership matching")
}

func TestMediaSearchPersonDirection(t *testing.T) {
	t.Parallel()
	for _, direction := range []string{"from_person", "to_person", "membership change", "person deleted", "all mapped", "multi mapped", "all hidden", "all bytes", "all late hidden", "all late bytes", "all wrong destination", "all late wrong destination", "all no audio", "competing delivery", "delivery input"} {
		t.Run(direction, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			seed := f.match(t, "sender")
			scoped := strings.HasPrefix(direction, "all ") || strings.HasPrefix(direction, "multi ")
			state := ""
			if scoped {
				state = strings.SplitN(direction, " ", 2)[1]
			}
			var other recordingSeed
			if state == "no audio" {
				other.messageID = f.message(t, "other")
				other.attachmentID, _ = f.storedAudio(t, other.messageID, "other", "")
			} else if scoped {
				other = f.match(t, "other")
			} else {
				other = f.retained(t, "other")
			}
			participant := f.f.EnsureParticipant("speaker@example.com", "Speaker", "example.com")
			person, _, err := f.f.Store.CreatePersonFromParticipant(participant)
			require.NoError(err)
			_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), participant, seed.messageID)
			require.NoError(err)
			if scoped {
				_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'to')`), other.messageID, participant)
				require.NoError(err)
				change := func() {
					var err error
					switch strings.TrimPrefix(state, "late ") {
					case "hidden":
						_, err = f.f.Store.MergeDuplicates(seed.messageID, []int64{other.messageID}, "batch")
					case "bytes":
						_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE attachments SET content_hash = ? WHERE id = ?`), strings.Repeat("c", 64), other.attachmentID)
					case "wrong destination":
						_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE beeper_media_occurrences SET destination_key = 'other-destination' WHERE attachment_id = ?`), other.attachmentID)
					case "no audio":
						_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE attachments SET filename = 'note.txt', mime_type = 'text/plain', media_type = 'file' WHERE id = ?`), other.attachmentID)
					}
					require.NoError(err)
				}
				if strings.HasPrefix(state, "late ") {
					f.duringSearch = change
				} else {
					change()
					if state != "mapped" {
						f.report.Results = f.report.Results[:1]
						f.report.MediaSelections = f.report.MediaSelections[:1]
						f.report.Coverage.ScopedDocuments, f.report.Coverage.CompleteDocuments = 1, 1
					}
				}
			}
			if direction == "competing delivery" || direction == "delivery input" {
				_, err := f.f.Store.DB().Exec(`UPDATE beeper_media_deliveries SET profile = 'supplied-transcript', supplied_input_id = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`)
				require.NoError(err)
				f.report.MediaSelections[0].Origin = "supplied"
				f.report.MediaSelections[0].SuppliedInputID = strings.Repeat("a", 64)
				f.duringSearch = func() {
					if direction == "competing delivery" {
						f.sameAudio["competitor"] = "sender"
						f.retained(t, "competitor")
						f.report.Results = []docbankmedia.SearchHit{}
						f.report.MediaSelections = []docbankmedia.SearchMediaSelection{}
						f.report.Coverage.State = "incomplete"
						f.report.Coverage.CompleteDocuments = 0
					} else {
						_, err := f.f.Store.DB().Exec(`UPDATE beeper_media_deliveries SET supplied_input_id = 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'`)
						assert.NoError(err)
					}
				}
			}
			relation := direction
			if direction == "competing delivery" || direction == "delivery input" {
				relation = "from_person"
			}
			if direction == "membership change" || direction == "person deleted" {
				relation = "from_person"
				if direction == "person deleted" {
					f.report.Results = []docbankmedia.SearchHit{}
				}
				f.duringSearch = func() {
					var err error
					if direction == "person deleted" {
						err = f.f.Store.DeletePersonContext(t.Context(), person.ID, person.Revision)
					} else {
						_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE messages SET sender_id = NULL WHERE id = ?`), seed.messageID)
					}
					require.NoError(err)
				}
			}
			query := fmt.Sprintf("q=quarterly+numbers&person_id=%d", person.ID)
			if strings.HasPrefix(direction, "multi ") {
				query += "&direction=from_person,to_person"
			} else if !scoped {
				query += "&direction=" + relation
			}
			status, response, raw := searchMediaFor(t, f.server(true), query)
			if direction == "person deleted" {
				assert.Equal(http.StatusNotFound, status, raw)
				assert.Contains(raw, "person_not_found")
				return
			}
			require.Equal(http.StatusOK, status, raw)
			if scoped {
				wantFence := []string{seed.contentVersionID}
				if state == "mapped" || strings.HasPrefix(state, "late ") {
					wantFence = append(wantFence, other.contentVersionID)
				}
				require.Len(f.requests, 1)
				assert.ElementsMatch(wantFence, f.requests[0].Fence.ContentVersionIDs)
				wantResults := 1
				if state == "mapped" {
					wantResults = 2
				}
				assert.Len(response.Results, wantResults)
				for _, result := range response.Results {
					assert.True(result.MessageID == seed.messageID || state == "mapped" && result.MessageID == other.messageID)
				}
				gap := 0
				if strings.HasSuffix(state, "bytes") || strings.HasSuffix(state, "wrong destination") {
					gap = 1
				}
				assert.Equal(gap, response.UnavailableOccurrences)
				assert.Zero(response.PendingOccurrences)
				return
			}
			switch direction {
			case "from_person":
				require.Len(response.Results, 1)
				assert.Equal(seed.messageID, response.Results[0].MessageID)
				assert.NotEqual(other.messageID, response.Results[0].MessageID)
				assert.Equal([]string{seed.contentVersionID}, f.requests[0].Fence.ContentVersionIDs)
			case "membership change", "competing delivery", "delivery input":
				assert.Empty(response.Results)
				assert.True(response.Partial)
				assert.Equal([]string{seed.contentVersionID}, f.requests[0].Fence.ContentVersionIDs)
			default:
				assert.Empty(response.Results)
				assert.Equal(1, f.validated)
			}
		})
	}
}

func TestMediaSearchSelectedProfilesForSharedContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	generated := f.match(t, "generated")
	f.transcripts["supplied"] = "current supplied quarterly numbers"
	f.sameAudio["supplied"] = "generated"
	messageID := f.message(t, "supplied")
	supplied := f.audio(t, messageID, "supplied", "", "delivery-supplied", &store.BeeperMediaResult{VaultUID: "vault", DocbankSourceID: "other-source", ContentVersionID: generated.contentVersionID})
	_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE beeper_media_deliveries SET phase = 'blocked' WHERE processing_key = ?`), "delivery-supplied")
	require.NoError(err)
	f.report.Results[0].Evidence[0].BuildID = "current-asr"
	f.report.Results[0].Excerpt = "current generated quarterly numbers"
	f.report.MediaSelections = append(f.report.MediaSelections, docbankmedia.SearchMediaSelection{SourceID: supplied.sourceID, SourceVersionID: "version", ContentVersionID: supplied.contentVersionID, Origin: "supplied", SuppliedInputID: supplied.suppliedInputID, Completeness: "complete"})
	f.report.Results = append(f.report.Results, docbankmedia.SearchHit{VaultUID: "vault", NodeID: 1, ContentVersionID: supplied.contentVersionID, Rank: 2, Excerpt: "current supplied quarterly numbers", Evidence: []docbankmedia.SearchEvidence{{Kind: "rendition_segment", BuildID: "current-supplied", SegmentID: "segment-supplied", MediaSources: []docbankmedia.SearchMediaSource{{SourceID: supplied.sourceID, SourceVersionID: "version", ContentVersionID: supplied.contentVersionID}}, TimeSpan: &docbankmedia.MediaTimeSpan{StartMS: 50, EndMS: 100}}}})
	status, response, raw := searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	require.Equal(http.StatusOK, status, raw)
	require.Len(response.Results, 2)
	assert.Equal(generated.messageID, response.Results[0].MessageID)
	assert.Equal("generated", response.Results[0].Origin)
	assert.Equal(supplied.messageID, response.Results[1].MessageID)
	assert.Equal("supplied", response.Results[1].Origin)
	assert.Equal("current supplied quarterly numbers", response.Results[1].Excerpt)
	assert.False(response.Partial)
	assert.Zero(response.AttributionUnavailable)
	require.Len(f.requests, 1)
	assert.Len(f.requests[0].MediaSources, 2)
	assert.Len(f.requests[0].Fence.ContentVersionIDs, 1)
}

func TestMediaSearchRemoteErrors(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"503", "timeout"} {
		t.Run(name, func(t *testing.T) {
			f := newMediaSearchFixture(t)
			f.match(t, "matched")
			f.remoteStatus = http.StatusServiceUnavailable
			if name == "timeout" {
				f.remoteStatus = 0
				f.waitForCancel = true
			}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/media/search?q=words", nil)
			r.Header.Set("X-Api-Key", recordingsTestAPIKey)
			if name == "timeout" {
				ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the remote blocks until cancellation
				defer cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			f.server(true).Router().ServeHTTP(w, r)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			if name == "timeout" {
				assert.Contains(t, w.Body.String(), "query_timeout")
			} else {
				assert.Contains(t, w.Body.String(), "media_search_unavailable")
			}
		})
	}
}

func TestMediaSearchBulkRevisionEligibility(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"provider text", "supplied provider text", "supplied missing raw"} {
		t.Run(change, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f := newMediaSearchFixture(t)
			seed := f.match(t, "matched")
			supplied := strings.HasPrefix(change, "supplied ")
			mutation := strings.TrimPrefix(change, "supplied ")
			if supplied {
				_, err := f.f.Store.DB().Exec(`UPDATE beeper_media_deliveries SET profile = 'supplied-transcript'`)
				require.NoError(err)
				f.report.MediaSelections[0].Origin = "supplied"
				f.report.MediaSelections[0].SuppliedInputID = seed.suppliedInputID
			}
			if !supplied && change == "provider text" {
				_, err := f.f.Store.DB().Exec(`UPDATE beeper_media_deliveries SET profile = 'supplied-transcript', phase = 'pending-process', operation_state = 'queued'`)
				require.NoError(err)
			}
			var err error
			switch mutation {
			case "provider text":
				f.transcripts["matched"] = "new transcript"
				f.writeRaw(t, seed.messageID, "matched")
			case "missing raw":
				_, err = f.f.Store.DB().Exec(`DELETE FROM message_raw`)
			}
			require.NoError(err)
			if mutation == "provider text" || mutation == "missing raw" {
				if supplied {
					f.report.MediaSelections = []docbankmedia.SearchMediaSelection{}
					f.report.Results = []docbankmedia.SearchHit{}
					f.report.Coverage.State = "incomplete"
					f.report.Coverage.CompleteDocuments = 0
				}
				status, response, raw := searchMediaFor(t, f.server(true), "q=words")
				require.Equal(http.StatusOK, status, raw)
				if supplied {
					assert.Empty(response.Results)
					require.Len(f.requests, 1)
					assert.Equal([]string{}, f.requests[0].MediaSources[0].SuppliedInputIDs)
					assert.True(response.Partial)
				} else {
					require.Len(response.Results, 1)
					assert.Equal(seed.messageID, response.Results[0].MessageID)
					assert.Len(f.requests, 1)
					assert.False(response.Partial)
					if change == "provider text" {
						assert.Empty(f.requests[0].MediaSources[0].SuppliedInputIDs)
					}
				}
			}
		})
	}
}

func TestMediaSearchUnreadySelectionKeepsReadyMatches(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"pending-process", "observing", "done", "missing revision", "generated pending", "hidden", "asr pending-process", "asr pending-process opt out"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			ready := f.match(t, "ready")
			unready := f.retained(t, "pending-selection")
			phase := state
			consent := !strings.HasSuffix(state, "opt out")
			if strings.HasPrefix(state, "asr ") {
				phase = strings.Fields(state)[1]
				_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE beeper_media_deliveries SET profile = 'configured-asr', supplied_input_id = '' WHERE processing_key = ?`), "delivery-pending-selection")
				require.NoError(err)
			}
			if state == "missing revision" || strings.HasPrefix(state, "generated ") || state == "hidden" {
				phase = "pending-process"
			}
			_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE beeper_media_deliveries SET phase = ?, operation_state = 'queued' WHERE processing_key = ?`), phase, "delivery-pending-selection")
			require.NoError(err)
			f.report.Coverage = docbankmedia.SearchCoverage{State: "incomplete", ScopedDocuments: 2, CompleteDocuments: 1}
			pending, unavailable := 1, 0
			if state == "done" || state == "missing revision" || !consent {
				pending, unavailable = 0, 1
			}
			if state == "missing revision" {
				require.NoError(f.f.Store.UpsertMessageRawWithFormat(unready.messageID, []byte("invalid"), "beeper_json"))
			}
			if strings.HasPrefix(state, "generated ") {
				f.report.MediaSelections = append(f.report.MediaSelections, docbankmedia.SearchMediaSelection{SourceID: unready.sourceID, SourceVersionID: "version", ContentVersionID: unready.contentVersionID, Origin: "generated", Completeness: "complete"})
				f.report.Coverage.State, f.report.Coverage.CompleteDocuments = "complete", 2
				pending = 0
			}
			if state == "hidden" {
				f.duringSearch = func() {
					_, err := f.f.Store.MergeDuplicates(ready.messageID, []int64{unready.messageID}, "batch")
					require.NoError(err)
				}
				pending = 0
			}
			queries := []string{"q=quarterly+numbers"}
			if state == "pending-process" {
				queries = append(queries, "q=absent+words")
			}
			for _, query := range queries {
				status, response, raw := searchMediaFor(t, f.server(consent), query)
				require.Equal(http.StatusOK, status, raw)
				assert.Equal(pending, response.PendingOccurrences)
				assert.Equal(unavailable, response.UnavailableOccurrences)
				assert.Zero(response.AttributionUnavailable)
				assert.Equal(!strings.HasPrefix(state, "generated "), response.Partial)
				if query == "q=quarterly+numbers" {
					require.Len(response.Results, 1)
					assert.Equal(ready.messageID, response.Results[0].MessageID)
					require.Len(f.requests, 1)
					assert.Contains(f.requests[0].Fence.ContentVersionIDs, unready.contentVersionID)
				} else {
					assert.Empty(response.Results)
				}
				f.report.Results = []docbankmedia.SearchHit{}
				f.duringSearch = nil
			}
		})
	}
}

func TestMediaSearchLocalGapsUseDurableWork(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"unmapped", "mapped", "another destination", "skipped"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f := newMediaSearchFixture(t)
			id := f.message(t, "pending")
			switch state {
			case "mapped":
				f.audio(t, id, "pending", "", "", nil)
			case "skipped":
				attachmentID, _ := f.storedAudio(t, id, "pending", "")
				_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE attachments SET attachment_state = 'skipped', content_hash = NULL, storage_path = '' WHERE id = ?`), attachmentID)
				require.NoError(err)
			case "another destination":
				seed := f.audio(t, id, "pending", "", "", &store.BeeperMediaResult{VaultUID: "vault"})
				_, err := f.f.Store.DB().Exec(`UPDATE sources SET source_type = 'gmail'`)
				require.NoError(err)
				_, err = f.f.Store.DB().Exec(`UPDATE beeper_media_occurrences SET source_type = 'gmail', destination_key = 'other-destination'`)
				require.NoError(err)
				_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE attachments SET filename = 'header.bin', mime_type = 'application/octet-stream', media_type = '' WHERE id = ?`), seed.attachmentID)
				require.NoError(err)
			default:
				attachmentID, _ := f.storedAudio(t, id, "pending", "")
				_, err := f.f.Store.DB().Exec(`UPDATE sources SET source_type = 'gmail'`)
				require.NoError(err)
				_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE attachments SET filename = 'note.ogg', mime_type = 'application/octet-stream', attachment_role = 'unknown' WHERE id = ?`), attachmentID)
				require.NoError(err)
			}
			status, response, raw := searchMediaFor(t, f.server(true), "q=words")
			require.Equal(http.StatusOK, status, raw)
			assert.True(response.Partial)
			if state == "mapped" {
				assert.Equal(1, response.PendingOccurrences)
				assert.Zero(response.UnavailableOccurrences)
			} else {
				assert.Zero(response.PendingOccurrences)
				assert.Equal(1, response.UnavailableOccurrences)
			}
			assert.Empty(f.requests)
		})
	}
}

func TestMediaSearchSharedSourceAllowedInputs(t *testing.T) {
	for _, test := range []struct {
		name              string
		hits, attribution int
		partial           bool
	}{
		{"caption A only", 0, 1, true}, {"neither caption", 0, 1, true}, {"caption B", 1, 1, true},
		{"generated", 2, 0, false}, {"generated nohit", 0, 0, false}, {"partial generated", 0, 0, true}, {"degraded provenance", 0, 0, true}, {"late caption", 0, 2, true}, {"hide unmatched", 0, 0, true}, {"hide selected", 0, 1, true}, {"no-hit bytes", 0, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			f.transcripts["first-input"] = "caption A"
			first := f.retained(t, "first-input")
			f.sameAudio["second-input"] = "first-input"
			f.transcripts["second-input"] = "caption B"
			secondID := f.message(t, "second-input")
			second := f.audio(t, secondID, "second-input", "", "delivery-second-input", &store.BeeperMediaResult{VaultUID: "vault", DocbankSourceID: first.sourceID, ContentVersionID: first.contentVersionID})
			source := docbankmedia.SearchMediaSource{SourceID: first.sourceID, SourceVersionID: "version", ContentVersionID: first.contentVersionID}
			selection := docbankmedia.SearchMediaSelection{SearchMediaSource: source, Origin: "supplied", SuppliedInputID: second.suppliedInputID, Completeness: "complete"}
			if strings.HasPrefix(test.name, "generated") || test.name == "partial generated" || test.name == "degraded provenance" {
				selection.Origin, selection.SuppliedInputID = "generated", ""
			}
			if test.name == "partial generated" {
				selection.Completeness = "partial"
			}
			if test.name == "degraded provenance" {
				selection.Completeness = "degraded_provenance"
			}
			f.report.MediaSelections = []docbankmedia.SearchMediaSelection{selection}
			f.report.Coverage = docbankmedia.SearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}
			if test.hits > 0 {
				f.report.Results = []docbankmedia.SearchHit{{VaultUID: "vault", NodeID: 1, ContentVersionID: first.contentVersionID, Rank: 1, Excerpt: "selected caption B", Evidence: []docbankmedia.SearchEvidence{{Kind: "rendition_segment", BuildID: "build", SegmentID: "segment", MediaSources: []docbankmedia.SearchMediaSource{source}}}}}
			}
			f.duringSearch = func() {
				var err error
				switch test.name {
				case "late caption":
					f.transcripts["second-input"] = "edited caption B"
					f.writeRaw(t, second.messageID, "second-input")
				case "hide unmatched", "hide selected":
					hidden := first.messageID
					if test.name == "hide selected" {
						hidden = second.messageID
					}
					_, err = f.f.Store.MergeDuplicates(f.message(t, "survivor"), []int64{hidden}, "hidden")
				case "no-hit bytes":
					_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE attachments SET content_hash = ? WHERE id = ?`), strings.Repeat("c", 64), second.attachmentID)
				}
				require.NoError(err)
			}
			status, response, raw := searchMediaFor(t, f.server(true), "q="+url.QueryEscape(test.name))
			require.Equal(http.StatusOK, status, raw)
			require.Len(f.requests, 1)
			require.Len(f.requests[0].MediaSources, 1)
			assert.ElementsMatch([]string{first.suppliedInputID, second.suppliedInputID}, f.requests[0].MediaSources[0].SuppliedInputIDs)
			assert.Len(response.Results, test.hits)
			assert.Equal(test.attribution, response.AttributionUnavailable)
			assert.Equal(test.partial, response.Partial)
			if test.name == "caption B" {
				assert.Equal(second.messageID, response.Results[0].MessageID)
			}
		})
	}
}

func TestMediaSearchSuppliedInputLimit(t *testing.T) {
	for _, test := range []struct {
		name          string
		inputs        int
		ordinary      bool
		pending       bool
		sharedVersion bool
	}{
		{"64 inputs", 64, false, false, false},
		{"65 inputs and ordinary recording", 65, true, false, false},
		{"65 inputs with pending caption", 65, false, true, false},
		{"65 inputs and shared content version", 65, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			f.transcripts["first-input"] = "caption A"
			first := f.retained(t, "first-input")
			inputs := []string{first.suppliedInputID}
			for i := 1; i < test.inputs; i++ {
				id := fmt.Sprintf("caption-%d", i)
				f.transcripts[id] = id
				f.sameAudio[id] = "first-input"
				seed := f.audio(t, f.message(t, id), id, "", "delivery-"+id, &store.BeeperMediaResult{VaultUID: "vault", DocbankSourceID: first.sourceID, ContentVersionID: first.contentVersionID})
				f.deliver(t, "delivery-"+id, "succeeded")
				if test.pending && i == 64 {
					_, err := f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE beeper_media_deliveries SET phase = 'pending-process', operation_state = 'queued' WHERE processing_key = ?`), "delivery-"+id)
					require.NoError(err)
				}
				inputs = append(inputs, seed.suppliedInputID)
			}
			// A repeated caption on another occurrence uses the same allowed input.
			f.transcripts["duplicate"] = "caption A"
			f.sameAudio["duplicate"] = "first-input"
			f.audio(t, f.message(t, "duplicate"), "duplicate", "", "delivery-first-input", &store.BeeperMediaResult{VaultUID: "vault", DocbankSourceID: first.sourceID, ContentVersionID: first.contentVersionID})
			selected := first
			if test.sharedVersion {
				selected = f.audio(t, f.message(t, "ordinary"), "ordinary", "", "delivery-ordinary", &store.BeeperMediaResult{VaultUID: "vault", DocbankSourceID: "ordinary-source", ContentVersionID: first.contentVersionID})
				f.deliver(t, "delivery-ordinary", "succeeded")
				source := docbankmedia.SearchMediaSource{SourceID: selected.sourceID, SourceVersionID: "version", ContentVersionID: selected.contentVersionID}
				f.report.MediaSelections = []docbankmedia.SearchMediaSelection{{SearchMediaSource: source, Origin: "supplied", SuppliedInputID: selected.suppliedInputID, Completeness: "complete"}}
				f.report.Results = []docbankmedia.SearchHit{{VaultUID: "vault", NodeID: 1, ContentVersionID: selected.contentVersionID, Rank: 1, Excerpt: "ordinary caption", Evidence: []docbankmedia.SearchEvidence{{Kind: "rendition_segment", BuildID: "build", SegmentID: "segment", MediaSources: []docbankmedia.SearchMediaSource{source}}}}}
			} else if test.ordinary {
				selected = f.match(t, "ordinary")
			}
			if test.inputs == 64 {
				f.report.MediaSelections = []docbankmedia.SearchMediaSelection{{SourceID: first.sourceID, SourceVersionID: "version", ContentVersionID: first.contentVersionID, Origin: "supplied", SuppliedInputID: first.suppliedInputID, Completeness: "complete"}}
			}
			status, response, raw := searchMediaFor(t, f.server(true), "q=words")
			require.Equal(http.StatusOK, status, raw)
			if test.inputs == 65 {
				assert.Equal(test.inputs+1, response.UnavailableOccurrences)
				assert.True(response.Partial)
			} else {
				assert.Zero(response.UnavailableOccurrences)
			}
			assert.Zero(response.PendingOccurrences)
			if test.inputs == 65 && !test.ordinary {
				assert.Empty(f.requests)
				assert.Equal(1, f.validated)
				assert.Empty(response.Results)
				return
			}
			assert.Zero(f.validated)
			require.Len(f.requests, 1)
			assert.Equal([]string{selected.contentVersionID}, f.requests[0].Fence.ContentVersionIDs)
			require.Len(f.requests[0].MediaSources, 1)
			assert.Equal(docbankmedia.SearchMediaSource{SourceID: selected.sourceID, SourceVersionID: "version", ContentVersionID: selected.contentVersionID}, f.requests[0].MediaSources[0].SearchMediaSource)
			if test.ordinary {
				assert.Equal([]string{selected.suppliedInputID}, f.requests[0].MediaSources[0].SuppliedInputIDs)
				require.Len(response.Results, 1)
				assert.Equal(selected.messageID, response.Results[0].MessageID)
			} else {
				assert.ElementsMatch(inputs, f.requests[0].MediaSources[0].SuppliedInputIDs)
			}
		})
	}
}

func (f *mediaSearchFixture) document(t *testing.T, server *Server) (int64, int64) {
	t.Helper()
	first, second := f.message(t, "first-document"), f.message(t, "second-document")
	hash := strings.Repeat("a", 64)
	for _, mid := range []int64{first, second} {
		require.NoError(t, f.f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: "report.pdf", StoragePath: "aa/" + hash, State: "stored"}))
	}
	require.NoError(t, f.f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash, MIMEType: "application/pdf"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: "00000000-0000-4000-8000-000000000001"}, ""))
	server.docbankAttachments = &DocbankAttachmentReader{Store: f.f.Store, Client: f.client, Destination: "attachments"}
	f.documentReport = &docbankmedia.SearchReport{RequestedMode: "lexical", ActualMode: "lexical", Coverage: docbankmedia.SearchCoverage{State: "unknown"}, Results: []docbankmedia.SearchHit{{VaultUID: "00000000-0000-4000-8000-000000000010", NodeID: 7, ContentVersionID: "00000000-0000-4000-8000-000000000001", Rank: 1, Excerpt: "quartz in the PDF", Evidence: []docbankmedia.SearchEvidence{{Kind: "rendition_segment", BuildID: "build", SegmentID: "segment"}}}}}
	return first, second
}

func TestMediaSearchDocumentsShared(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMediaSearchFixture(t)
	recording := f.match(t, "audio")
	server := f.server(false)
	first, second := f.document(t, server)
	query := "q=quartz"
	hash := strings.Repeat("b", 64)
	staleDocumentVersion := "00000000-0000-4000-8000-000000000002"
	require.NoError(f.f.Store.UpsertAttachmentRecord(t.Context(), first, store.AttachmentWrite{ContentHash: hash, Filename: "stale.pdf", StoragePath: "bb/" + hash, State: "stored"}))
	require.NoError(f.f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash, MIMEType: "application/pdf"}, store.DocbankAttachmentRef{NodeID: 8, VersionID: staleDocumentVersion}, ""))
	status, response, raw := searchMediaFor(t, server, query)
	require.Equal(http.StatusOK, status, raw)
	require.NotNil(response.Attachments)
	require.Len(response.Results, 1)
	assert.Equal(recording.messageID, response.Results[0].MessageID)
	assert.Equal("generated", response.Results[0].Origin)
	assert.False(response.Attachments.Truncated)
	require.Nil(response.Attachments.Error, raw)
	require.Len(response.Attachments.Results, 2)
	assert.ElementsMatch([]int64{first, second}, []int64{response.Attachments.Results[0].MessageID, response.Attachments.Results[1].MessageID})
	for _, hit := range response.Attachments.Results {
		assert.Equal("quartz in the PDF", hit.Excerpt)
	}
}

func TestMediaSearchDocumentsOccurrenceLimit(t *testing.T) {
	for _, test := range []struct {
		name             string
		query            string
		docbankTruncated bool
		results          int
	}{
		{"occurrence limit", "q=quartz&limit=1", false, 1},
		{"Docbank limit", "q=quartz", true, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			f.match(t, "audio")
			server := f.server(false)
			f.document(t, server)
			f.documentReport.Truncated = test.docbankTruncated
			status, response, raw := searchMediaFor(t, server, test.query)
			require.Equal(http.StatusOK, status, raw)
			require.NotNil(response.Attachments)
			assert.True(response.Attachments.Truncated)
			require.Len(response.Attachments.Results, test.results)
			for _, hit := range response.Attachments.Results {
				assert.Equal("quartz in the PDF", hit.Excerpt)
			}
		})
	}
}

func TestMediaSearchDocumentsAttachmentScopeLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMediaSearchFixture(t)
	f.match(t, "audio")
	server := f.server(false)
	first, _ := f.document(t, server)
	var documents []storetest.DocumentAttachment
	for i := 2; i <= 4097; i++ {
		documents = append(documents, storetest.DocumentAttachment{MIMEType: "application/pdf", VersionID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i)})
	}
	f.f.SeedDocumentAttachments(first, documents)
	status, response, raw := searchMediaFor(t, server, "q=quartz")
	require.Equal(http.StatusOK, status, raw)
	require.NotNil(response.Attachments)
	require.NotNil(response.Attachments.Error)
	assert.Empty(response.Attachments.Results)
	assert.Equal("media_search_scope_limit", response.Attachments.Error.Error)
	assert.Contains(response.Attachments.Error.Message, "4096 mirrored files excluding images, audio, and video")
	assert.Contains(response.Attachments.Error.Message, "--person")
	assert.False(response.Attachments.Truncated)
	assert.Empty(f.documentRequests)
}

func TestMediaSearchDocumentsAttachmentFailure(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"unavailable", http.StatusServiceUnavailable, ""},
		{"rejected query", http.StatusBadRequest, "invalid_media_search"},
		{"store failure", 0, "server_error"},
		{"late store failure", 0, "server_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			f.match(t, "audio")
			server := f.server(false)
			f.document(t, server)
			f.documentStatus = test.status
			storeFailure := func() {
				_, err := f.f.Store.DB().Exec(`ALTER TABLE docbank_attachment_deliveries RENAME TO unavailable_attachment_deliveries`)
				require.NoError(err)
			}
			switch test.name {
			case "store failure":
				storeFailure()
			case "late store failure":
				f.duringDocumentSearch = storeFailure
			}
			status, response, raw := searchMediaFor(t, server, "q=quartz")
			require.Equal(http.StatusOK, status, raw)
			require.NotNil(response.Attachments)
			require.NotNil(response.Attachments.Error)
			assert.Empty(response.Attachments.Results)
			assert.NotEmpty(response.Results)
			if test.code != "" {
				assert.Equal(test.code, response.Attachments.Error.Error)
			}
		})
	}
}

func TestMediaSearchDocumentsEmptyScope(t *testing.T) {
	for _, name := range []string{"no versions in scope", "no Docbank hits"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			f.match(t, "audio")
			server := f.server(false)
			if name == "no versions in scope" {
				server.docbankAttachments = &DocbankAttachmentReader{Store: f.f.Store, Client: f.client, Destination: "attachments"}
			} else {
				f.document(t, server)
				f.documentReport.Results = []docbankmedia.SearchHit{}
			}
			status, response, raw := searchMediaFor(t, server, "q=quartz")
			require.Equal(http.StatusOK, status, raw)
			require.NotNil(response.Attachments)
			assert.Nil(response.Attachments.Error)
			assert.Empty(response.Attachments.Results)
			assert.False(response.Attachments.Truncated)
		})
	}
}

func TestMediaSearchDocumentsRecordingFailure(t *testing.T) {
	for _, test := range []struct {
		name            string
		recordingStatus int
		documentStatus  int
		timeout         bool
		status          int
	}{
		{"timeout", 0, 0, true, http.StatusOK},
		{"unavailable", http.StatusServiceUnavailable, 0, false, http.StatusOK},
		{"late store failure", 0, 0, false, http.StatusOK},
		{"rejected query", http.StatusBadRequest, 0, false, http.StatusBadRequest},
		{"both fail", http.StatusServiceUnavailable, http.StatusServiceUnavailable, false, http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			f.match(t, "audio")
			server := f.server(false)
			f.document(t, server)
			f.remoteStatus, f.documentStatus = test.recordingStatus, test.documentStatus
			f.waitForCancel = test.timeout
			if test.name == "late store failure" {
				f.report.Truncated = true
				f.duringSearch = func() {
					_, err := f.f.Store.DB().Exec(`ALTER TABLE beeper_media_occurrences RENAME TO unavailable_media_occurrences`)
					assert.NoError(err)
				}
			}
			ctx := t.Context()
			if test.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 4*time.Second)
				defer cancel()
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/media/search?q=quartz", nil).WithContext(ctx)
			request.Header.Set("X-Api-Key", recordingsTestAPIKey)
			recorder := httptest.NewRecorder()
			server.Router().ServeHTTP(recorder, request)
			raw := recorder.Body.String()
			require.Equal(test.status, recorder.Code, raw)
			if test.status == http.StatusBadRequest {
				assert.Contains(raw, `"error":"invalid_media_search"`)
				return
			}
			if test.status != http.StatusOK {
				return
			}
			var response MediaSearchResponse
			require.NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
			require.NotNil(response.Attachments)
			require.NotNil(response.RecordingsError)
			assert.Equal(docbankmedia.SearchCoverage{State: "unknown"}, response.Coverage)
			assert.False(response.Truncated)
			assert.Zero(response.PendingOccurrences)
			assert.Zero(response.UnavailableOccurrences)
			assert.Zero(response.AttributionUnavailable)
			assert.Empty(response.Results)
			assert.True(response.Partial)
			assert.False(response.Attachments.Truncated)
			require.Len(response.Attachments.Results, 2)
			for _, hit := range response.Attachments.Results {
				assert.Equal("quartz in the PDF", hit.Excerpt)
			}
		})
	}
}

func TestMediaSearchDocumentsUnlinkedDuringSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMediaSearchFixture(t)
	f.match(t, "audio")
	server := f.server(false)
	first, second := f.document(t, server)
	query := "q=quartz"
	participant := f.f.EnsureParticipant("reader@example.com", "Reader", "example.com")
	alias := f.f.EnsureParticipant("alias@example.com", "Alias", "example.com")
	_, err := f.f.Store.LinkParticipants(participant, alias)
	require.NoError(err)
	_, err = f.f.Store.DB().Exec(f.f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id IN (?,?)`), alias, first, second)
	require.NoError(err)
	query += "&participant_id=" + strconv.FormatInt(participant, 10)
	f.duringDocumentSearch = func() {
		_, err := f.f.Store.UnlinkParticipants(participant, alias)
		assert.NoError(err)
	}
	status, response, raw := searchMediaFor(t, server, query)
	require.Equal(http.StatusOK, status, raw)
	require.NotNil(response.Attachments)
	assert.Empty(response.Results)
	assert.False(response.Attachments.Truncated)
	require.Empty(response.Attachments.Results)
}

// Local attribution can outlast the remote budget after a slow successful response.
func TestMediaSearchLocalMappingAfterDocbankBudget(t *testing.T) {
	for _, kind := range []string{"recording", "attachment"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaSearchFixture(t)
			recording := f.match(t, "audio")
			server := f.server(false)
			remoteCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			slowCtx, slowCancel := context.WithTimeout(t.Context(), time.Second)
			defer slowCancel()
			slow := func() { <-slowCtx.Done() }
			resolve := func() (*personscope.Scope, error) {
				// The request still has time for local queries when Docbank's budget ends.
				<-remoteCtx.Done()
				return nil, nil //nolint:nilnil // No Person filter searches the full visible archive.
			}
			if kind == "recording" {
				f.duringSearch = slow
				response, err := server.messageRecordings.search(t.Context(), remoteCtx, "quartz", 20, nil, resolve)
				require.NoError(err)
				require.Len(response.Results, 1)
				assert.Equal(recording.messageID, response.Results[0].MessageID)
			} else {
				first, second := f.document(t, server)
				f.duringDocumentSearch = slow
				response, err := server.docbankAttachments.SearchAttachments(t.Context(), remoteCtx, "quartz", 20, nil, resolve)
				require.NoError(err)
				assert.Nil(response.Error)
				var messages []int64
				for _, hit := range response.Results {
					messages = append(messages, hit.MessageID)
				}
				assert.ElementsMatch([]int64{first, second}, messages)
			}
		})
	}
}

func TestMediaSearchReplacesLocalDocumentRoute(t *testing.T) {
	f := newMediaSearchFixture(t)
	server := f.server(false)
	retired := doGet(server, "/api/v1/documents/search?q=synthetic")
	assert.Equal(t, http.StatusNotFound, retired.Code)
	code, _, raw := searchMediaFor(t, server, "q=synthetic")
	assert.Equal(t, http.StatusOK, code, raw)
}
