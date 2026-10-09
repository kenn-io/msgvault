package api

import (
	"context"
	"encoding/json"
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
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

type mediaSearchFixture struct {
	*recordingFixture

	report        docbankmedia.SearchReport
	requests      []docbankmedia.SearchRequest
	validated     int
	duringSearch  func()
	remoteStatus  int
	waitForCancel bool
}

func newMediaSearchFixture(t *testing.T) *mediaSearchFixture {
	t.Helper()
	f := &mediaSearchFixture{recordingFixture: newRecordingFixture(t), report: docbankmedia.SearchReport{
		MediaSourceSelection: true, MediaSelections: []docbankmedia.SearchMediaSelection{}, RequestedMode: "lexical", ActualMode: "lexical", Coverage: docbankmedia.SearchCoverage{State: "complete"}, Results: []docbankmedia.SearchHit{},
	}}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
	status, response, raw := searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	require.Equal(http.StatusOK, status, raw)
	require.Len(response.Results, 2)
	assert.Equal(seed.messageID, response.Results[0].MessageID)
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
	for i := range 1001 {
		f.retained(t, "recording-"+strconv.Itoa(i))
	}
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
	t.Parallel()
	for _, query := range []string{"q=words&mode=semantic", "q=words&mode=hybrid", "q=words&mode=invalid", "q=words&limit=101", "q=words&person_id=0", "q=words&direction=from_person", "q=words&person_id=7&direction=invalid", "q=words&person_id=7&direction=", "q=" + url.QueryEscape(strings.Repeat("x", 8193))} {
		f := newMediaSearchFixture(t)
		status, _, raw := searchMediaFor(t, f.server(true), query)
		want := http.StatusBadRequest
		if strings.Contains(query, "mode=semantic") || strings.Contains(query, "mode=hybrid") {
			want = http.StatusServiceUnavailable
		}
		assert.Equal(t, want, status, raw)
		assert.Empty(t, f.requests)
	}
	f := newMediaSearchFixture(t)
	w := httptest.NewRecorder()
	f.server(true).Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/media/search?q=words", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMediaSearchOversizedPopulation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	f := newMediaSearchFixture(t)
	f.match(t, "seed")
	// Clone mapped identities to exercise the public fence ceiling without thousands of imports.
	transaction, err := f.f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = transaction.Rollback() })
	for i := range docbankmedia.MaxSearchVersions {
		_, err := transaction.Exec(f.f.Store.Rebind(`INSERT INTO beeper_media_occurrences (
			destination_key, occurrence_ref, revision, source_type, source_identifier, source_conversation_id,
			source_message_id, source_attachment_id, source_part_key, source_sha256, byte_length,
			occurrence_json, request_filename, request_mime_type, retention_state, vault_uid,
			source_id, source_version_id, content_version_id, processing_key)
			SELECT destination_key, ?, revision, source_type, source_identifier, source_conversation_id,
			source_message_id, source_attachment_id, source_part_key, source_sha256, byte_length,
			occurrence_json, request_filename, request_mime_type, retention_state, vault_uid,
			source_id, source_version_id, ?, processing_key
			FROM beeper_media_occurrences WHERE occurrence_ref = 'msgvault:seed'`), fmt.Sprintf("msgvault:copy-%d", i), fmt.Sprintf("content-%d", i))
		require.NoError(err)
	}
	require.NoError(transaction.Commit())
	status, _, raw := searchMediaFor(t, f.server(true), "q=quarterly+numbers")
	assert.Equal(http.StatusBadRequest, status, raw)
	assert.Contains(raw, "media_search_scope_limit")
	assert.Empty(f.requests)
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
		{"generated", 2, 0, false}, {"generated nohit", 0, 0, false}, {"partial generated", 0, 0, true}, {"late caption", 0, 2, true}, {"hide unmatched", 0, 0, true}, {"hide selected", 0, 1, true}, {"no-hit bytes", 0, 1, true},
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
			if strings.HasPrefix(test.name, "generated") || test.name == "partial generated" {
				selection.Origin, selection.SuppliedInputID = "generated", ""
			}
			if test.name == "partial generated" {
				selection.Completeness = "partial"
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
