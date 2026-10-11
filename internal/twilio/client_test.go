package twilio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAC = "ACaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testCA = "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testRE = "REaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testGT = "GTaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testClient(t *testing.T, handler http.HandlerFunc, change func(*Options)) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	options := Options{AccountSID: testAC, AuthToken: "test-secret", Endpoints: map[string]string{"voice": server.URL, "intelligence": server.URL}}
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
func TestVoiceRejectsUnsafePageAndForeignAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"page", map[string]any{"recordings": []any{}, "next_page_uri": "https://example.com/2010-04-01/Accounts/" + testAC + "/Recordings.json"}},
		{"page outside the collection", map[string]any{"recordings": []any{}, "next_page_uri": "/2010-04-01/Accounts/" + testAC + "/Calls.json"}},
		{"foreign", map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": "ACbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "call_sid": testCA}}}},
		{"schema", map[string]any{"wrong": []any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, tc.value) }, nil)
			_, _, err := c.ListRecordingsPage(context.Background(), time.Time{}, 1000, "")
			require.Error(t, err)
		})
	}
}

// Legacy and Intelligence transcripts are read per recording: sentences in
// index order with their offsets, a participant role naming the speaker, and
// a completed transcript with no speech noted as unavailable.
func TestClassicAndLegacyTranscripts(t *testing.T) {
	offset := 1.25
	segment := func(require *require.Assertions, evidence Evidence) Segment {
		for _, tr := range evidence.Transcripts {
			if tr.Kind == "classic" {
				require.NotEmpty(tr.Segments)
				return tr.Segments[0]
			}
		}
		require.Fail("no classic transcript")
		return Segment{}
	}
	noSpeech := "classic intelligence unavailable: completed transcript " + testGT + " has no speech"
	emptyCase := func(status string) func(*assert.Assertions, *require.Assertions, Evidence) {
		return func(assert *assert.Assertions, require *require.Assertions, evidence Evidence) {
			require.Len(evidence.Transcripts, 1)
			assert.Equal(status == "completed", evidence.Transcripts[0].Complete)
			assert.False(evidence.Transcripts[0].Usable)
			if status == "completed" {
				assert.Contains(evidence.Diagnostics, noSpeech)
			} else {
				assert.NotContains(evidence.Diagnostics, noSpeech)
			}
		}
	}
	for _, tc := range []struct {
		name        string
		legacy      []any
		status      string
		participant bool
		sentences   []any
		check       func(*assert.Assertions, *require.Assertions, Evidence)
	}{
		{"legacy and ordered sentences",
			[]any{map[string]any{"sid": "TRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "account_sid": testAC, "recording_sid": testRE, "status": "completed", "date_created": "Sat, 03 Oct 2026 10:05:00 +0000", "transcription_text": "Legacy text"}},
			"completed", false,
			[]any{map[string]any{"sentence_index": 2, "media_channel": 2, "start_time": 1.25, "transcript": "Second"}, map[string]any{"sentence_index": 1, "media_channel": 1, "start_time": 0, "transcript": "First"}},
			func(assert *assert.Assertions, require *require.Assertions, evidence Evidence) {
				require.Len(evidence.Transcripts, 2)
				assert.True(evidence.Transcripts[0].Complete)
				assert.True(evidence.Transcripts[0].Usable)
				var classic, legacy Transcript
				for _, tr := range evidence.Transcripts {
					if tr.Kind == "classic" {
						classic = tr
					} else {
						legacy = tr
					}
				}
				assert.Equal("2026-10-03T10:06:00Z", classic.DateCreated)
				assert.Equal("Sat, 03 Oct 2026 10:05:00 +0000", legacy.DateCreated)
				require.Len(classic.Segments, 2)
				assert.Equal("First", classic.Segments[0].Text)
				assert.Equal("channel 1", classic.Segments[0].Speaker)
			}},
		{"completed with no speech", nil, "completed", false, nil, emptyCase("completed")},
		{"queued", nil, "queued", false, nil, emptyCase("queued")},
		{"in progress", nil, "in-progress", false, nil, emptyCase("in-progress")},
		{"string offsets", nil, "completed", false,
			[]any{map[string]any{"sentence_index": 0, "media_channel": 1, "start_time": "1.250", "end_time": "2.0", "transcript": "text"}},
			func(assert *assert.Assertions, require *require.Assertions, evidence Evidence) {
				got := segment(require, evidence)
				require.NotNil(got.OffsetSeconds)
				assert.InDelta(offset, *got.OffsetSeconds, 1e-9)
				assert.Contains(string(evidence.Transcripts[0].Raw), "source_sid", "provider metadata is preserved")
			}},
		{"null offsets and participant role", nil, "completed", true,
			[]any{map[string]any{"sentence_index": 0, "media_channel": "1", "start_time": nil, "end_time": nil, "transcript": "text"}},
			func(assert *assert.Assertions, require *require.Assertions, evidence Evidence) {
				got := segment(require, evidence)
				assert.Equal("Customer", got.Speaker)
				assert.Nil(got.OffsetSeconds)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			channel := map[string]any{"media_properties": map[string]any{"source_sid": testRE}}
			if tc.participant {
				channel["participants"] = []any{map[string]any{"channel_participant": 1, "role": "Customer", "media_participant_id": "+18005550100"}}
			}
			legacy, sentences := tc.legacy, tc.sentences
			if legacy == nil {
				legacy = []any{}
			}
			if sentences == nil {
				sentences = []any{}
			}
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + "/Transcriptions.json":
					writeJSON(t, w, map[string]any{"transcriptions": legacy})
				case "/v2/Transcripts":
					assert.Equal(testRE, r.URL.Query().Get("SourceSid"))
					writeJSON(t, w, map[string]any{"transcripts": []any{map[string]any{"sid": testGT, "account_sid": testAC, "channel": channel, "status": tc.status, "date_created": "2026-10-03T10:06:00Z"}}})
				case "/v2/Transcripts/" + testGT + "/Sentences":
					assert.Empty(r.URL.Query().Get("Redacted"))
					writeJSON(t, w, map[string]any{"sentences": sentences})
				default:
					assert.Fail("unexpected endpoint", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}, nil)
			evidence, err := c.Transcripts(context.Background(), []Recording{{SID: testRE, AccountSID: testAC, CallSID: testCA}})
			require.NoError(err)
			tc.check(assert, require, evidence)
		})
	}
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
				c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					assert.NotEqual("/v2/Transcripts", r.URL.Path, "Intelligence is read in US1 only")
					writeJSON(t, w, map[string]any{"transcriptions": []any{}})
				}, func(options *Options) { options.Region = tc.region })
				evidence, err := c.Transcripts(context.Background(), []Recording{{SID: testRE, CallSID: testCA}})
				require.NoError(err)
				assert.Empty(evidence.Diagnostics, "a region fact isn't stored per call")
			}
		})
	}
}

// A refused transcript read (401/403) leaves that transcript unavailable with
// a note instead of failing the call, configured service or not.
func TestTranscriptAuthFailures(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		legacy     bool
		configured bool
		status     int
	}{
		{"legacy 401", "legacy transcription coverage unavailable (HTTP 401)", true, false, http.StatusUnauthorized},
		{"legacy 403", "legacy transcription coverage unavailable (HTTP 403)", true, false, http.StatusForbidden},
		{"optional intelligence 403", "classic intelligence coverage unavailable (HTTP 403)", false, false, http.StatusForbidden},
		{"configured intelligence 403", "classic intelligence coverage unavailable (HTTP 403)", false, true, http.StatusForbidden},
		{"malformed legacy answer fails the call", "legacy transcription acquisition failed", true, false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/Transcriptions.json") == tc.legacy {
					w.WriteHeader(tc.status)
					return
				}
				writeJSON(t, w, map[string]any{"transcriptions": []any{}, "transcripts": []any{}})
			}, func(options *Options) {
				if tc.configured {
					options.IntelligenceServiceSID = "GAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				}
			})
			evidence, err := c.Transcripts(context.Background(), []Recording{{SID: testRE, CallSID: testCA}})
			if tc.status == http.StatusOK {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Contains(t, evidence.Diagnostics, tc.want)
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
func TestListRecordingsPageResumesWithItsFilterWindow(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	requests := 0
	path := "/2010-04-01/Accounts/" + testAC + "/Recordings.json"
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(http.MethodGet, r.Method)
		u, p, ok := r.BasicAuth()
		assert.True(ok)
		assert.Equal(testAC, u)
		assert.Equal("test-secret", p)
		assert.Equal(path, r.URL.Path)
		assert.Equal("1000", r.URL.Query().Get("PageSize"))
		assert.Equal("2026-10-01", r.URL.Query().Get("DateCreated>"))
		assert.Equal("true", r.URL.Query().Get("IncludeSoftDeleted"))
		switch r.URL.Query().Get("Page") {
		case "", "0":
			recording := map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA, "channels": 2}
			writeJSON(t, w, map[string]any{
				"recordings":    []any{recording, recording},
				"next_page_uri": path + "?DateCreated%3E=2026-10-01&IncludeSoftDeleted=true&PageSize=1000&Page=1",
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
	require.Len(first, 1, "a recording listed twice is kept once")
	assert.Equal(2, first[0].Channels)
	require.NotEmpty(cursor)
	assert.Equal(1, requests, "a page read should stop after one provider response")
	second, cursor, err := c.ListRecordingsPage(context.Background(), after, 1000, cursor)
	require.NoError(err)
	require.Len(second, 1)
	assert.Empty(cursor)
	assert.Equal(2, requests)
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

func TestNewClientRejectsInvalidCredentialsWithoutEchoingThem(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options Options
		want    string
	}{
		{"account", Options{AccountSID: "AC123", AuthToken: "do-not-print-this"}, "account_sid"},
		{"missing credential", Options{AccountSID: testAC}, "api_key_sid"},
		{"key without secret", Options{AccountSID: testAC, APIKeySID: "SK" + strings.Repeat("0", 32)}, "api_key_secret"},
		{"mixed credentials", Options{AccountSID: testAC, APIKeySID: "SK" + strings.Repeat("0", 32), APIKeySecret: "another-private-secret", AuthToken: "do-not-print-this"}, "mutually exclusive"},
		{"region", Options{AccountSID: testAC, AuthToken: "do-not-print-this", Region: "xx1"}, "region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(tc.options)
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "do-not-print-this")
			assert.NotContains(t, err.Error(), "another-private-secret")
		})
	}
}
