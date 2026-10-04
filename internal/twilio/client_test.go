package twilio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAC = "ACaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testCA = "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testRE = "REaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testGT = "GTaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testVX = "VXaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testClient(t *testing.T, handler http.HandlerFunc, change func(*Options)) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	options := Options{AccountSID: testAC, AuthToken: "test-secret", Endpoints: map[string]string{"voice": server.URL, "intelligence": server.URL, "insights": server.URL, "batch": server.URL, "orchestrator": server.URL}}
	if change != nil {
		change(&options)
	}
	c, err := NewClient(options)
	require.NoError(t, err)
	return c
}
func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(value))
}
func TestVoicePaginationAndValidation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		u, p, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, testAC, u)
		assert.Equal(t, "test-secret", p)
		if r.URL.Query().Get("PageToken") == "two" {
			writeJSON(t, w, map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA}}})
			return
		}
		assert.Equal(t, "2026-10-01", r.URL.Query().Get("DateCreated>="))
		writeJSON(t, w, map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA, "channels": 2}}, "next_page_uri": "/2010-04-01/Accounts/" + testAC + "/Recordings.json?PageToken=two"})
	}, nil)
	recordings, err := c.ListRecordings(context.Background(), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Len(t, recordings, 1)
	assert.Equal(t, 2, recordings[0].Channels)
}
func TestVoiceRejectsUnsafePageAndForeignAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"page", map[string]any{"recordings": []any{}, "next_page_uri": "https://example.com/2010-04-01/Accounts/" + testAC + "/Recordings.json"}},
		{"foreign", map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": "ACbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "call_sid": testCA}}}},
		{"schema", map[string]any{"wrong": []any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, tc.value) }, nil)
			_, err := c.ListRecordings(context.Background(), time.Time{})
			require.Error(t, err)
		})
	}
}
func TestClassicAndLegacyTranscripts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + "/Transcriptions.json":
			writeJSON(t, w, map[string]any{"transcriptions": []any{map[string]any{"sid": "TRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "account_sid": testAC, "recording_sid": testRE, "status": "completed", "transcription_text": "Legacy text"}}})
		case "/v2/Transcripts":
			assert.Equal(testRE, r.URL.Query().Get("SourceSid"))
			writeJSON(t, w, map[string]any{"transcripts": []any{map[string]any{"sid": testGT, "account_sid": testAC, "channel": map[string]any{"media_properties": map[string]any{"source_sid": testRE}}, "status": "completed"}}})
		case "/v2/Transcripts/" + testGT:
			writeJSON(t, w, map[string]any{"sid": testGT, "account_sid": testAC, "channel": map[string]any{"media_properties": map[string]any{"source_sid": testRE}}, "status": "completed"})
		case "/v2/Transcripts/" + testGT + "/Sentences":
			assert.Empty(r.URL.Query().Get("Redacted"))
			writeJSON(t, w, map[string]any{"sentences": []any{map[string]any{"sentence_index": 2, "media_channel": 2, "start_time": 1.25, "transcript": "Second"}, map[string]any{"sentence_index": 1, "media_channel": 1, "start_time": 0, "transcript": "First"}}})
		case "/v3/Transcriptions":
			writeJSON(t, w, map[string]any{"transcriptions": []any{}})
		case "/v2/Conversations":
			writeJSON(t, w, map[string]any{"conversations": []any{}})
		default:
			assert.Fail("unexpected endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}, nil)
	evidence, err := c.Transcripts(context.Background(), Call{SID: testCA, AccountSID: testAC}, []Recording{{SID: testRE, AccountSID: testAC, CallSID: testCA}})
	require.NoError(err)
	require.Len(evidence.Transcripts, 2)
	assert.True(evidence.Transcripts[0].Complete)
	assert.True(evidence.Transcripts[0].Usable)
	var classic Transcript
	for _, tr := range evidence.Transcripts {
		if tr.Kind == "classic" {
			classic = tr
		}
	}
	require.Len(classic.Segments, 2)
	assert.Equal("First", classic.Segments[0].Text)
	assert.Equal("channel 1", classic.Segments[0].Speaker)
}
func TestOrchestratorTokenPagingAndCallScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(testCA, r.URL.Query().Get("channelId"))
		if r.URL.Path == "/v2/Conversations" {
			writeJSON(t, w, map[string]any{"conversations": []any{map[string]any{"id": "conv_example", "accountId": testAC}}})
			return
		}
		assert.Equal("/v2/Conversations/conv_example/Communications", r.URL.Path)
		text := "First"
		meta := map[string]any{"nextToken": "next"}
		id := "comm_first"
		if r.URL.Query().Get("pageToken") == "next" {
			text = "Second"
			meta = map[string]any{}
			id = "comm_second"
		}
		writeJSON(t, w, map[string]any{"communications": []any{map[string]any{"provider_extension": "preserved", "id": id, "accountId": testAC, "conversationId": "conv_example", "channelId": testCA, "resourceId": "opaque-resource", "content": map[string]any{"type": "TEXT", "text": text}, "author": map[string]any{"participantId": "participant_example"}, "occurredAt": "2026-10-03T10:00:00Z"}}, "meta": meta})
	}, nil)
	evidence, err := c.Transcripts(context.Background(), Call{SID: testCA, AccountSID: testAC}, nil)
	require.NoError(err)
	require.Len(evidence.Transcripts, 1)
	assert.Len(evidence.Transcripts[0].Segments, 2)
	assert.Nil(evidence.Transcripts[0].Segments[0].OffsetSeconds)
	assert.NotNil(evidence.Transcripts[0].Segments[0].StartedAt)
	assert.Contains(string(evidence.Transcripts[0].Raw), "provider_extension")
	require.Len(evidence.Conversations, 1)
	assert.Contains(string(evidence.Conversations[0]), "conv_example")
}
func TestUnsupportedRegionsDoNotFallback(t *testing.T) {
	for _, tc := range []struct{ region, origin string }{{"us1", "https://api.twilio.com"}, {"ie1", "https://api.dublin.ie1.twilio.com"}, {"au1", "https://api.sydney.au1.twilio.com"}} {
		t.Run(tc.region, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			c, err := NewClient(Options{AccountSID: testAC, AuthToken: "test", Region: tc.region})
			require.NoError(err)
			assert.Equal(tc.origin, c.origins["voice"].String())
			if tc.region != "us1" {
				evidence, err := c.Transcripts(context.Background(), Call{SID: testCA}, nil)
				require.NoError(err)
				assert.NotEmpty(evidence.Diagnostics)
			}
		})
	}
}

func TestBatchContractFetchEnvelopeAndPaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/Transcriptions":
			assert.Equal(testRE, r.URL.Query().Get("sourceId"))
			assert.Equal("100", r.URL.Query().Get("pageSize"))
			if r.URL.Query().Get("pageToken") == "next" {
				writeJSON(t, w, map[string]any{"transcriptions": []any{}})
				return
			}
			writeJSON(t, w, map[string]any{"transcriptions": []any{map[string]any{"id": "voice_transcription_example", "accountId": testAC, "sourceId": testRE, "status": "COMPLETED"}}, "meta": map[string]any{"nextToken": "next"}})
		case "/v3/Transcriptions/voice_transcription_example":
			writeJSON(t, w, map[string]any{"operationId": "voice_transcription_example", "status": "COMPLETED", "transcription": map[string]any{"id": "voice_transcription_example", "accountId": testAC, "sourceId": testRE, "status": "COMPLETED", "conversationId": "conv_example"}})
		case "/v2/Conversations/conv_example/Communications":
			assert.Equal(testCA, r.URL.Query().Get("channelId"))
			writeJSON(t, w, map[string]any{"communications": []any{map[string]any{"id": "comm_example", "conversationId": "conv_example", "accountId": testAC, "channelId": testCA, "content": map[string]any{"type": "TEXT", "text": "Retained batch text"}}}})
		default:
			assert.Fail("unexpected Batch endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}, nil)
	transcripts, diagnostics, err := c.batch(context.Background(), testCA, testRE)
	require.NoError(err)
	assert.Empty(diagnostics)
	require.Len(transcripts, 2)
	var batch, callView Transcript
	for _, tr := range transcripts {
		switch tr.Kind {
		case "batch":
			batch = tr
		case "orchestrator":
			callView = tr
		}
	}
	assert.True(batch.Complete)
	assert.False(batch.Usable)
	assert.Empty(batch.Segments)
	assert.Empty(batch.Communications)
	assert.Contains(string(batch.Raw), "conv_example")
	assert.Equal(testCA, callView.SourceID)
	assert.Equal("conv_example", callView.ID)
	require.Len(callView.Segments, 1)
	assert.Equal("Retained batch text", callView.Segments[0].Text)
}
func TestClassicStringOffsets(t *testing.T) {
	require := require.New(t)

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		metadata := map[string]any{"sid": testGT, "account_sid": testAC, "channel": map[string]any{"media_properties": map[string]any{"source_sid": testRE}}, "status": "completed"}
		switch r.URL.Path {
		case "/v2/Transcripts":
			writeJSON(t, w, map[string]any{"transcripts": []any{metadata}})
		case "/v2/Transcripts/" + testGT:
			writeJSON(t, w, metadata)
		case "/v2/Transcripts/" + testGT + "/Sentences":
			writeJSON(t, w, map[string]any{"sentences": []any{map[string]any{"sentence_index": 0, "media_channel": 1, "start_time": "1.250", "end_time": "2.0", "transcript": "String timestamps"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}, nil)
	transcripts, err := c.classic(context.Background(), testRE)
	require.NoError(err)
	require.Len(transcripts, 1)
	require.NotNil(transcripts[0].Segments[0].OffsetSeconds)
	assert.InDelta(t, 1.25, *transcripts[0].Segments[0].OffsetSeconds, 1e-9)
}
func TestRelayCarrierEdgeAndSessionScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		metadata := map[string]any{"sid": testGT, "account_sid": testAC, "channel": map[string]any{"media_properties": map[string]any{"source_sid": testVX}}, "status": "completed"}
		switch r.URL.Path {
		case "/v1/Voice/" + testCA + "/Events":
			assert.Equal("carrier_edge", r.URL.Query().Get("Edge"))
			writeJSON(t, w, map[string]any{"events": []any{map[string]any{"call_sid": testCA, "account_sid": testAC, "conversation_relay_data": map[string]any{"session_id": testVX}}}})
		case "/v2/Transcripts":
			assert.Equal(testVX, r.URL.Query().Get("SourceSid"))
			writeJSON(t, w, map[string]any{"transcripts": []any{metadata}})
		case "/v2/Transcripts/" + testGT:
			writeJSON(t, w, metadata)
		case "/v2/Transcripts/" + testGT + "/Sentences":
			writeJSON(t, w, map[string]any{"sentences": []any{map[string]any{"sentence_index": 0, "media_channel": 1, "start_time": 0, "transcript": "Relay text"}}})
		case "/v2/Conversations":
			writeJSON(t, w, map[string]any{"conversations": []any{}})
		default:
			assert.Fail("unexpected Relay endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}, func(options *Options) { options.RelayDiscovery = true })
	evidence, err := c.Transcripts(context.Background(), Call{SID: testCA}, nil)
	require.NoError(err)
	require.Len(evidence.Transcripts, 1)
	assert.Equal(testVX, evidence.Transcripts[0].Segments[0].Scope)
	assert.True(c.DiscoverCalls())
	require.Len(evidence.RelayEvents, 1)
	assert.Contains(string(evidence.RelayEvents[0]), testVX)
}
func TestCommunicationsRejectsForeignCallAtomically(t *testing.T) {
	assert := assert.New(t)

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		callID := testCA
		if r.URL.Query().Get("pageToken") == "next" {
			callID = "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}
		meta := map[string]any{}
		if callID == testCA {
			meta["nextToken"] = "next"
		}
		writeJSON(t, w, map[string]any{"communications": []any{map[string]any{"id": "comm_" + callID, "conversationId": "conv_example", "accountId": testAC, "channelId": callID, "content": map[string]any{"type": "TEXT", "text": "A page"}}}, "meta": meta})
	}, nil)
	transcript, err := c.communications(context.Background(), testCA, "conv_example", "orchestrator", testCA)
	require.Error(t, err)
	assert.False(transcript.Complete)
	assert.False(transcript.Usable)
	assert.Empty(transcript.Segments)
}

func TestCompleteEmptyAndPendingEvidence(t *testing.T) {
	for _, status := range []string{"completed", "queued", "in-progress"} {
		t.Run(status, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				metadata := map[string]any{"sid": testGT, "channel": map[string]any{"media_properties": map[string]any{"source_sid": testRE}}, "account_sid": testAC, "status": status}
				switch r.URL.Path {
				case "/v2/Transcripts":
					writeJSON(t, w, map[string]any{"transcripts": []any{metadata}})
				case "/v2/Transcripts/" + testGT:
					writeJSON(t, w, metadata)
				case "/v2/Transcripts/" + testGT + "/Sentences":
					writeJSON(t, w, map[string]any{"sentences": []any{}})
				default:
					assert.Fail("unexpected endpoint", r.URL.Path)
				}
			}, nil)
			transcripts, err := c.classic(context.Background(), testRE)
			require.NoError(err)
			require.Len(transcripts, 1)
			assert.Equal(status == "completed", transcripts[0].Complete)
			assert.False(transcripts[0].Usable)
		})
	}
}
func TestOptionalCoverageAndConfiguredAuthFailure(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "configured"}[configured], func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/Transcripts" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if r.URL.Path == "/v3/Transcriptions" {
					writeJSON(t, w, map[string]any{"transcriptions": []any{}})
					return
				}
				if r.URL.Path == "/v2/Conversations" {
					writeJSON(t, w, map[string]any{"conversations": []any{}})
					return
				}
				writeJSON(t, w, map[string]any{"transcriptions": []any{}})
			}, func(options *Options) {
				if configured {
					options.IntelligenceServiceSID = "GAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				}
			})
			evidence, err := c.Transcripts(context.Background(), Call{SID: testCA}, []Recording{{SID: testRE, CallSID: testCA}})
			if configured {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Contains(t, evidence.Diagnostics, "classic intelligence coverage unavailable (HTTP 403)")
		})
	}
}
func TestRetry429AndCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(t, w, map[string]any{"sid": testCA, "account_sid": testAC})
	}, nil)
	call, err := c.GetCall(context.Background(), testCA)
	require.NoError(err)
	assert.Equal(testCA, call.SID)
	assert.Equal(3, requests)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.GetCall(ctx, testCA)
	require.ErrorIs(err, context.Canceled)
}
func TestPaginationLoopAndFilterChanges(t *testing.T) {
	for _, next := range []string{"/2010-04-01/Accounts/" + testAC + "/Recordings.json?PageSize=1000", "/2010-04-01/Accounts/" + testAC + "/Recordings.json?DateCreated%3E%3D=2020-01-01", "/2010-04-01/Accounts/" + testAC + "/Calls.json"} {
		t.Run(next, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, map[string]any{"recordings": []any{}, "next_page_uri": next})
			}, nil)
			_, err := c.ListRecordings(context.Background(), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
			require.Error(t, err)
		})
	}
}

func TestListRecordingsPageResumesWithItsFilterWindow(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	requests := 0
	path := "/2010-04-01/Accounts/" + testAC + "/Recordings.json"
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(path, r.URL.Path)
		assert.Equal("1000", r.URL.Query().Get("PageSize"))
		assert.Equal("2026-10-01", r.URL.Query().Get("DateCreated>="))
		switch r.URL.Query().Get("Page") {
		case "", "0":
			writeJSON(t, w, map[string]any{
				"recordings":    []any{map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA}},
				"next_page_uri": path + "?PageSize=1000&Page=1",
			})
		case "1":
			writeJSON(t, w, map[string]any{
				"recordings":    []any{map[string]any{"sid": "REbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "account_sid": testAC, "call_sid": testCA}},
				"next_page_uri": nil,
			})
		default:
			assert.Fail("unexpected recordings page", r.URL.RawQuery)
		}
	}, nil)

	first, cursor, err := c.ListRecordingsPage(context.Background(), after, 1000, "")
	require.NoError(err)
	require.Len(first, 1)
	require.NotEmpty(cursor)
	assert.Equal(1, requests, "a page read should stop after one provider response")
	second, cursor, err := c.ListRecordingsPage(context.Background(), after, 1000, cursor)
	require.NoError(err)
	require.Len(second, 1)
	assert.Empty(cursor)
	assert.Equal(2, requests)
}

func TestListCallsPageResumesWithItsFilterWindow(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	requests := 0
	path := "/2010-04-01/Accounts/" + testAC + "/Calls.json"
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertions.Equal(path, r.URL.Path)
		assertions.Equal("17", r.URL.Query().Get("PageSize"))
		assertions.Equal("2026-10-01", r.URL.Query().Get("StartTime>="))
		switch r.URL.Query().Get("Page") {
		case "", "0":
			writeJSON(t, w, map[string]any{
				"calls":         []any{map[string]any{"sid": testCA, "account_sid": testAC, "date_created": "Thu, 01 Oct 2026 00:00:00 +0000"}},
				"next_page_uri": path + "?Page=1&PageSize=17",
			})
		case "1":
			writeJSON(t, w, map[string]any{
				"calls":         []any{map[string]any{"sid": "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "account_sid": testAC, "date_created": "Fri, 02 Oct 2026 00:00:00 +0000"}},
				"next_page_uri": nil,
			})
		default:
			assertions.Fail("unexpected calls page", r.URL.RawQuery)
		}
	}, nil)
	first, cursor, err := client.ListCallsPage(context.Background(), after, 17, "")
	requirements.NoError(err)
	requirements.Len(first, 1)
	requirements.NotEmpty(cursor)
	assertions.Equal(testCA, first[0].SID)
	assertions.Equal(1, requests, "a page read must stop after one provider response")
	second, cursor, err := client.ListCallsPage(context.Background(), after, 17, cursor)
	requirements.NoError(err)
	requirements.Len(second, 1)
	assertions.Empty(cursor)
	assertions.Equal("CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", second[0].SID)
	assertions.Equal(2, requests)
}

func TestVoiceCallEndpoints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		call := map[string]any{"sid": testCA, "account_sid": testAC, "parent_call_sid": "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "from": "+18005550100", "to": "+18005550101", "status": "completed", "duration": "65", "date_created": "Thu, 01 Oct 2026 00:00:00 +0000"}
		switch r.URL.Path {
		case "/2010-04-01/Accounts/" + testAC + "/Calls.json":
			assert.Equal("2026-10-01", r.URL.Query().Get("StartTime>="))
			writeJSON(t, w, map[string]any{"calls": []any{call, call}})
		case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + ".json":
			writeJSON(t, w, call)
		case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + "/Recordings.json":
			writeJSON(t, w, map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA}}})
		default:
			assert.Fail("unexpected endpoint", r.URL.Path)
		}
	}, nil)
	calls, err := c.ListCalls(context.Background(), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(calls, 1)
	assert.Equal("65", calls[0].Duration)
	call, err := c.GetCall(context.Background(), testCA)
	require.NoError(err)
	assert.Equal(calls[0], call)
	recordings, err := c.CallRecordings(context.Background(), testCA)
	require.NoError(err)
	assert.Len(recordings, 1)
}

func TestListCallsFiltersByCallCreationDate(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("2026-10-01", r.URL.Query().Get("StartTime>="))
		writeJSON(t, w, map[string]any{"calls": []any{
			map[string]any{"sid": testCA, "account_sid": testAC, "date_created": "Wed, 30 Sep 2026 23:59:59 +0000", "start_time": "2026-10-01T00:00:00Z"},
			map[string]any{"sid": "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "account_sid": testAC, "date_created": "Thu, 01 Oct 2026 00:00:00 +0000", "start_time": "2026-10-01T00:00:00Z"},
			map[string]any{"sid": "CAcccccccccccccccccccccccccccccccc", "account_sid": testAC, "start_time": "2026-10-02T00:00:00Z"},
		}})
	}, nil)
	calls, err := c.ListCalls(context.Background(), after)
	require.NoError(err)
	require.Len(calls, 1)
	assert.Equal("CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", calls[0].SID)
}

func TestClassicOfficialNestedSourceAndParticipantRole(t *testing.T) {
	for _, sourceID := range []string{testRE, testVX} {
		t.Run(sourceID, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				metadata := map[string]any{"sid": testGT, "account_sid": testAC, "status": "completed", "channel": map[string]any{"media_properties": map[string]any{"source_sid": sourceID}, "participants": []any{map[string]any{"channel_participant": 1, "role": "Customer", "media_participant_id": "+18005550100"}}}}
				switch r.URL.Path {
				case "/v2/Transcripts":
					writeJSON(t, w, map[string]any{"transcripts": []any{metadata}})
				case "/v2/Transcripts/" + testGT:
					writeJSON(t, w, metadata)
				case "/v2/Transcripts/" + testGT + "/Sentences":
					writeJSON(t, w, map[string]any{"sentences": []any{map[string]any{"sid": "GXaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sentence_index": 0, "media_channel": "1", "start_time": nil, "end_time": nil, "transcript": "Official schema"}}})
				default:
					assert.Fail("unexpected endpoint", r.URL.Path)
				}
			}, nil)
			transcripts, err := c.classic(context.Background(), sourceID)
			require.NoError(err)
			require.Len(transcripts, 1)
			assert.Equal(sourceID, transcripts[0].SourceID)
			assert.Equal("Customer", transcripts[0].Segments[0].Speaker)
			assert.Nil(transcripts[0].Segments[0].OffsetSeconds)
			assert.Contains(string(transcripts[0].Raw), "media_participant_id")
		})
	}
}

func TestVoiceRawEvidenceSurvivesArchiveReload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	original := []byte(`{"sid":"` + testCA + `","account_sid":"` + testAC + `","provider_future_field":{"preserved":true}}`)
	var call Call
	require.NoError(json.Unmarshal(original, &call))
	assert.JSONEq(string(original), string(call.Raw))
	first, err := json.Marshal(call)
	require.NoError(err)
	var reloaded Call
	require.NoError(json.Unmarshal(first, &reloaded))
	second, err := json.Marshal(reloaded)
	require.NoError(err)
	assert.JSONEq(string(first), string(second))
	recordingOriginal := []byte(`{"sid":"` + testRE + `","account_sid":"` + testAC + `","provider_future_field":{"preserved":true}}`)
	var recording Recording
	require.NoError(json.Unmarshal(recordingOriginal, &recording))
	assert.JSONEq(string(recordingOriginal), string(recording.Raw))
}

func TestLegacyDetailFallbackValidatesIdentityAndPreservesRaw(t *testing.T) {
	const id = "TRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "foreign"}[foreign], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/2010-04-01/Accounts/"+testAC+"/Recordings/"+testRE+"/Transcriptions.json" {
					writeJSON(t, w, map[string]any{"transcriptions": []any{map[string]any{"sid": id, "recording_sid": testRE, "status": "completed"}}})
					return
				}
				detailID := id
				if foreign {
					detailID = "TRbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				}
				writeJSON(t, w, map[string]any{"sid": detailID, "account_sid": testAC, "recording_sid": testRE, "status": "completed", "transcription_text": "Existing text", "provider_detail_evidence": "preserved"})
			}, nil)
			transcripts, err := c.legacy(context.Background(), testRE)
			if foreign {
				require.Error(err)
				assert.Empty(transcripts)
				return
			}
			require.NoError(err)
			require.Len(transcripts, 1)
			assert.Contains(string(transcripts[0].Raw), "provider_detail_evidence")
		})
	}
}
