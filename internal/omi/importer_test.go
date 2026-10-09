package omi

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const fixture = `{"id":"meeting-1","created_at":"2026-01-01T12:00:00Z","started_at":"2026-01-01T12:00:00Z","finished_at":"2026-01-01T12:01:00Z","status":"completed","structured":{"title":"Planning","overview":"Discuss the synthetic roadmap","action_items":[{"description":"Prepare proposal","completed":false,"due_at":null}]},"transcript_segments":[{"text":"Searchable synthetic transcript","speaker":"SPEAKER_01","speaker_id":1,"speaker_name":"Synthetic Speaker","is_user":false,"start":0,"end":60}],"future_field":{"preserve":true}}`

func decodeFixture(t *testing.T, raw string) Conversation {
	t.Helper()
	var c Conversation
	require.NoError(t, json.Unmarshal([]byte(raw), &c))
	c.Raw = jsontext.Value(raw)
	return c
}

// This is the official DeveloperConversation projection, rather than the
// richer backend model: lifecycle status, sections, roster and owners are absent.
const summaryOnlyFixture = `{"id":"summary-only","created_at":"2026-01-01T12:00:00Z","started_at":null,"finished_at":null,"structured":{"title":"Summary only","overview":"Initialsummary","emoji":"","category":"other","action_items":[],"events":[]},"language":"en","source":"external_integration","transcript_segments":null,"geolocation":null,"folder_id":null,"folder_name":null}`

func TestImportSummaryOnlyContinuesHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	rawSummary := summaryOnlyFixture
	titleOnly := `{"id":"title-only","structured":{"title":"Title only","overview":"","action_items":[]},"transcript_segments":null}`
	older := strings.ReplaceAll(fixture, "meeting-1", "older")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOmiPage(w, r, []Conversation{decodeFixture(t, rawSummary), decodeFixture(t, older), decodeFixture(t, titleOnly)})
	}))
	defer server.Close()
	client := testOmiClient(t, server.URL, "omi_dev_synthetic")
	imp := NewImporter(st, client)
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(3), sum.MeetingsAdded)
	ids, err := st.MessageExistsBatch(src.ID, []string{"summary-only", "older", "title-only"})
	require.NoError(err)
	require.Len(ids, 3)
	raw, err := st.GetMessageRaw(ids["summary-only"])
	require.NoError(err)
	assert.JSONEq(rawSummary, string(raw))
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Equal(meetingcontent.StateUnavailable, content.Transcript.State)
	assert.Equal(meetingcontent.StateUnsupported, content.Notes.State)
	savedTitle, err := st.GetMessageRaw(ids["title-only"])
	require.NoError(err)
	assert.JSONEq(titleOnly, string(savedTitle))
	assert.Equal(meetingcontent.StateUnavailable, meetingcontent.Decode(RawFormat, savedTitle, nil).Transcript.State)
	rawSummary = strings.ReplaceAll(rawSummary, "Initialsummary", "Editedsummary")
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsUpdated)
	raw, err = st.GetMessageRaw(ids["summary-only"])
	require.NoError(err)
	assert.JSONEq(rawSummary, string(raw))
	if st.FTS5Available() && !st.IsPostgreSQL() {
		var hits int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Editedsummary'`).Scan(&hits))
		assert.Equal(1, hits)
	}
}

func TestImportUnavailableReplacementContinuesHistory(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, priorEmpty := range []bool{false, true} {
			for _, replacement := range []string{"null", "missing", "title-only"} {
				t.Run(fmt.Sprintf("full=%t/empty=%t/%s", full, priorEmpty, replacement), func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					st := testutil.NewTestStore(t)
					src, err := st.GetOrCreateSource(SourceType, "work")
					require.NoError(err)
					var fields map[string]jsontext.Value
					require.NoError(json.Unmarshal([]byte(fixture), &fields))
					if priorEmpty {
						fields["transcript_segments"] = jsontext.Value(`[]`)
					}
					original, err := json.Marshal(fields)
					require.NoError(err)
					pages := map[int][]Conversation{0: {decodeFixture(t, string(original))}}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						rows := orderedOmiRows(pages)
						serveOmiPage(w, r, rows)
					}))
					defer server.Close()
					client := testOmiClient(t, server.URL, "omi_dev_synthetic")
					imp := NewImporter(st, client)
					opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: full}
					_, err = imp.Import(context.Background(), opts)
					require.NoError(err)
					initialIDs, err := st.MessageExistsBatch(src.ID, []string{"meeting-1"})
					require.NoError(err)
					priorBody, err := st.GetMessageBodyText(initialIDs["meeting-1"])
					require.NoError(err)
					switch replacement {
					case "null":
						fields["transcript_segments"] = jsontext.Value(`null`)
					case "missing":
						delete(fields, "transcript_segments")
					case "title-only":
						delete(fields, "transcript_segments")
						fields["structured"] = jsontext.Value(`{"title":"Title only","overview":"","action_items":[]}`)
					}
					replaced, err := json.Marshal(fields)
					require.NoError(err)
					pages[0] = []Conversation{decodeFixture(t, string(replaced)), decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "same-page"))}
					pages[PageSize] = []Conversation{decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "older-page"))}
					sum, err := imp.Import(context.Background(), opts)
					require.NoError(err, "keeping an archived transcript is not a failure")
					assert.Zero(sum.Errors)

					assert.Equal(int64(2), sum.MeetingsAdded)
					assert.Zero(sum.MeetingsUpdated)
					ids, err := st.MessageExistsBatch(src.ID, []string{"meeting-1", "same-page", "older-page"})
					require.NoError(err)
					require.Len(ids, 3)
					raw, err := st.GetMessageRaw(ids["meeting-1"])
					require.NoError(err)
					assert.JSONEq(string(original), string(raw))
					body, err := st.GetMessageBodyText(ids["meeting-1"])
					require.NoError(err)
					assert.Equal(priorBody, body)
					if st.FTS5Available() && !st.IsPostgreSQL() {
						var hits int
						require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Badreplacement'`).Scan(&hits))
						assert.Zero(hits)
						require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Searchable'`).Scan(&hits))
						want := 3
						if priorEmpty {
							want = 2
						}
						assert.Equal(want, hits)
					}
					latest, err := st.GetLatestSync(src.ID)
					require.NoError(err)
					assert.Equal("completed", latest.Status)
				})
			}
		}
	}
}

type pageSource struct {
	pages   map[int][]Conversation
	failure error
	offsets []int
}

func (s *pageSource) ListConversations(_ context.Context, p ListParams) ([]Conversation, error) {
	s.offsets = append(s.offsets, p.Offset)
	if len(s.offsets) > 1 && s.failure != nil {
		return nil, s.failure
	}
	rows := orderedOmiRows(s.pages)
	return filterOmiPage(rows, p), nil
}

func TestImportArchiveRescanAndRepair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	source := &pageSource{pages: map[int][]Conversation{0: {decodeFixture(t, fixture)}}}
	imp := NewImporter(st, source)
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
	first, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	ids, err := st.MessageExistsBatch(src.ID, []string{"meeting-1"})
	require.NoError(err)
	id := ids["meeting-1"]
	require.NotZero(id)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.JSONEq(fixture, string(raw))
	content := meetingcontent.Decode(RawFormat, raw, nil)
	require.Len(content.Transcript.Segments, 1)
	assert.Equal("Synthetic Speaker", content.Transcript.Segments[0].Speaker)
	require.Len(content.Actions, 1)
	assert.Equal(meetingcontent.StatusPending, content.Actions[0].Status)
	require.NotNil(content.DurationSeconds)
	assert.InDelta(float64(60), *content.DurationSeconds, 0)
	if st.FTS5Available() && !st.IsPostgreSQL() {
		var hits int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'`).Scan(&hits))
		assert.Equal(1, hits)
	}
	fromMe, err := st.GetMessageIsFromMe(id)
	require.NoError(err)
	assert.False(fromMe, "archive ownership does not establish an organizer")
	second, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Zero(second.MeetingsAdded + second.MeetingsUpdated)
	changedRaw := strings.ReplaceAll(fixture, "Discuss the synthetic roadmap", "Edited old conversation")
	changed := decodeFixture(t, changedRaw)
	source.pages[0] = []Conversation{changed}
	third, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), third.MeetingsUpdated)
	contentRaw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Contains(string(contentRaw), "Edited old conversation")
	opts.Full = true
	repaired, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), repaired.MeetingsUpdated)
}

func TestImportAfterFiltersByCreationTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	after := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	conversation := func(id string, createdAt, startedAt time.Time) Conversation {
		raw := strings.NewReplacer(
			`"meeting-1"`, `"`+id+`"`,
			`"created_at":"2026-01-01T12:00:00Z"`, `"created_at":"`+createdAt.Format(time.RFC3339Nano)+`"`,
			`"started_at":"2026-01-01T12:00:00Z"`, `"started_at":"`+startedAt.Format(time.RFC3339Nano)+`"`,
		).Replace(fixture)
		return decodeFixture(t, raw)
	}
	pages := map[int][]Conversation{0: {
		conversation("late-created", after.Add(time.Hour), after.Add(-24*time.Hour)),
		conversation("at-cutoff", after, after.Add(-24*time.Hour)),
		conversation("old-created", after.Add(-time.Second), after.Add(24*time.Hour)),
	}}
	var offsets []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(after.Format(time.RFC3339Nano), r.URL.Query().Get("start_date"))
		assert.NotEmpty(r.URL.Query().Get("end_date"), "the fixed import boundary stabilizes pagination")
		offset, parseErr := strconv.Atoi(r.URL.Query().Get("offset"))
		if parseErr != nil {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		offsets = append(offsets, offset)
		serveOmiPage(w, r, orderedOmiRows(pages))
	}))
	defer server.Close()
	client := testOmiClient(t, server.URL, "omi_dev_synthetic")
	imp := NewImporter(st, client)
	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "work", AccountEmail: "owner@example.com", CreatedAfter: after,
	})
	require.NoError(err)
	assert.Equal(int64(2), sum.MeetingsAdded)
	ids, err := st.MessageExistsBatch(src.ID, []string{"late-created", "at-cutoff", "old-created"})
	require.NoError(err)
	assert.Contains(ids, "late-created", "a recent creation is included even when the meeting started earlier")
	assert.Contains(ids, "at-cutoff", "the creation-date bound is inclusive")
	assert.NotContains(ids, "old-created", "an old creation is excluded even when its meeting started later")
}

func TestClientHostedAndSelfHostedContract(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", tls), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/prefix/v1/dev/user/conversations", r.URL.Path)
				assert.Equal("Bearer omi_dev_synthetic", r.Header.Get("Authorization"))
				assert.Equal("true", r.URL.Query().Get("include_transcript"))
				assert.Equal("200", r.URL.Query().Get("limit"))
				assert.Equal("200", r.URL.Query().Get("offset"))
				assert.Equal("2026-01-01T00:00:00Z", r.URL.Query().Get("start_date"))
				assert.Equal("2026-01-02T00:00:00Z", r.URL.Query().Get("end_date"))
				_, _ = w.Write([]byte("[" + fixture + "]"))
			}))
			if tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			client := testOmiClient(t, server.URL+"/prefix/", "omi_dev_synthetic")
			client.http = server.Client()
			result, err := client.ListConversations(context.Background(), ListParams{
				Limit: 1000, Offset: 200,
				CreatedAfter:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				CreatedBefore: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			})
			require.NoError(err)
			require.Len(result, 1)
			assert.JSONEq(fixture, string(result[0].Raw))
		})
	}
}

func TestClientErrorsAndRetry(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{401, "private provider text", "Developer API key"}, {403, "private provider text", "conversations:read"}, {302, "", "HTTP 302"}, {200, "null", "JSON array"}, {200, `[{"id":""}]`, "no ID"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := testOmiClient(t, server.URL, "test").ListConversations(context.Background(), ListParams{})
			require.ErrorContains(err, tc.want)
			assert.NotContains(t, err.Error(), "private provider text")
		})
	}
	require := require.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	client := testOmiClient(t, server.URL, "test")
	_, err := client.ListConversations(context.Background(), ListParams{})
	require.NoError(err)
	assert.Equal(t, 2, requests)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.ListConversations(ctx, ListParams{})
	require.ErrorIs(err, context.Canceled)
}

func TestNormalizeBaseURL(t *testing.T) {
	url, err := NormalizeBaseURL("")
	require.NoError(t, err)
	assert.Equal(t, DefaultBaseURL, url)
	for _, tc := range []struct {
		url       string
		wantError bool
	}{
		{url: "file:///tmp/backend", wantError: true},
		{url: "https://user:secret@example.com", wantError: true},
		{url: "https://example.com?key=secret", wantError: true},
		{url: "https://example.com?", wantError: true},
		{url: "https://example.com/v1/dev", wantError: true},
		{url: "http://omi.example.com", wantError: true},
		{url: "http://10.0.0.1", wantError: true},
		{url: "http://[2001:db8::1]", wantError: true},
		{url: "http://localhost:8000"},
		{url: "http://127.0.0.1:8000"},
		{url: "http://[::1]:8000"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.url)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.url, got)
		})
	}
}

func TestImportLimitAndInvalidTranscript(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	completed := decodeFixture(t, fixture)
	source := &pageSource{pages: map[int][]Conversation{0: {completed}}}
	imp := NewImporter(st, source)
	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
	require.NoError(err)
	invalid := decodeFixture(t, `{"id":"meeting-1","created_at":"2026-01-01T12:00:00Z","structured":{"title":"Invalid replacement"},"transcript_segments":42}`)
	source.pages[0] = []Conversation{invalid}
	failed, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
	require.ErrorContains(err, "unavailable transcript evidence")
	assert.Zero(failed.MeetingsUpdated)
	existing, err := st.MessageExistsBatch(sum.SourceID, []string{"meeting-1"})
	require.NoError(err)
	raw, err := st.GetMessageRaw(existing["meeting-1"])
	require.NoError(err)
	assert.JSONEq(fixture, string(raw))
}

func TestImportIncrementalWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	conversation := func(id string, createdAt time.Time) Conversation {
		return decodeFixture(t, strings.NewReplacer(
			`"meeting-1"`, `"`+id+`"`,
			`"created_at":"2026-01-01T12:00:00Z"`, `"created_at":"`+createdAt.Format(time.RFC3339Nano)+`"`,
		).Replace(fixture))
	}
	day := func(d int) time.Time { return time.Date(2026, 1, d, 12, 0, 0, 0, time.UTC) }
	// Newest first, as the Developer API orders pages by created_at.
	history := []Conversation{conversation("old", day(1)), conversation("ancient", day(1).AddDate(0, -1, 0))}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOmiPage(w, r, history)
	}))
	defer server.Close()
	client := testOmiClient(t, server.URL, "omi_dev_synthetic")
	imp := NewImporter(st, client)
	run := func(opts ImportOptions) (int64, string) {
		t.Helper()
		opts.Identifier, opts.AccountEmail = "work", "owner@example.com"
		sum, err := imp.Import(context.Background(), opts)
		require.NoError(err)
		last, err := st.GetLastSuccessfulSync(src.ID)
		require.NoError(err)
		return sum.MeetingsProcessed, last.CursorAfter.String
	}

	processed, cursor := run(ImportOptions{})
	assert.Equal(int64(2), processed, "the first sync reads all history")
	assert.JSONEq(`{"version":1,"created_after":"2026-01-01T12:00:00Z"}`, cursor)

	history = append([]Conversation{conversation("new", day(5))}, history...)
	processed, cursor = run(ImportOptions{})
	assert.Equal(int64(2), processed, "later syncs stop 48 hours before the watermark")
	assert.JSONEq(`{"version":1,"created_after":"2026-01-05T12:00:00Z"}`, cursor)

	history = append([]Conversation{conversation("newest", day(10))}, history...)
	processed, cursor = run(ImportOptions{Limit: 1})
	assert.Equal(int64(1), processed)
	assert.JSONEq(`{"version":1,"created_after":"2026-01-05T12:00:00Z"}`, cursor, "a limited run keeps the watermark")

	processed, cursor = run(ImportOptions{Full: true})
	assert.Equal(int64(4), processed, "a full sync rescans all history")
	assert.JSONEq(`{"version":1,"created_after":"2026-01-10T12:00:00Z"}`, cursor)
	ids, err := st.MessageExistsBatch(src.ID, []string{"ancient", "old", "new", "newest"})
	require.NoError(err)
	assert.Len(ids, 4)
}

func TestPacedClientRequiresDirectory(t *testing.T) {
	_, err := NewPacedClient(DefaultBaseURL, "omi_dev_synthetic", "").ListConversations(t.Context(), ListParams{})
	require.ErrorContains(t, err, "pacing directory is required")
}

func TestImportRefusesUnsupportedStoredStateBeforeWrites(t *testing.T) {
	for _, format := range []string{"cursor", "checkpoint"} {
		for _, version := range []string{"0", "2", "missing", "undecodable"} {
			t.Run(format+"/"+version, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st := testutil.NewTestStore(t)
				src, err := st.GetOrCreateSource(SourceType, "work")
				require.NoError(err)
				provider := &pageSource{pages: map[int][]Conversation{0: {decodeFixture(t, fixture)}}}
				imp := NewImporter(st, provider)
				_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
				require.NoError(err)
				last, err := st.GetLastSuccessfulSync(src.ID)
				require.NoError(err)
				payload := last.CursorAfter.String
				if format == "checkpoint" {
					payload = last.CursorBefore.String
				}
				switch version {
				case "undecodable":
					payload = "{"
				case "missing":
					payload = strings.Replace(payload, `"version":1`, `"draft":true`, 1)
				default:
					payload = strings.Replace(payload, `"version":1`, `"version":`+version, 1)
				}
				id, err := st.StartSync(src.ID, SourceType)
				require.NoError(err)
				if format == "cursor" {
					require.NoError(st.CompleteSync(id, payload))
				} else {
					require.NoError(st.FailSyncWithCheckpoint(id, "synthetic interruption", &store.Checkpoint{PageToken: payload}))
				}
				var runs, identities int
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&runs))
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM account_identities`).Scan(&identities))
				ids, err := st.MessageExistsBatch(src.ID, []string{"meeting-1"})
				require.NoError(err)
				provider.offsets = nil
				_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "other@example.com", Full: true, Limit: 1})
				require.Error(err)
				if version != "undecodable" {
					require.ErrorContains(err, "unsupported Omi")
				} else {
					require.ErrorContains(err, "decode Omi")
				}
				assert.Empty(provider.offsets)
				var gotRuns, gotIdentities int
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&gotRuns))
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM account_identities`).Scan(&gotIdentities))
				assert.Equal(runs, gotRuns)
				assert.Equal(identities, gotIdentities)
				raw, err := st.GetMessageRaw(ids["meeting-1"])
				require.NoError(err)
				assert.JSONEq(fixture, string(raw))
				body, err := st.GetMessageBodyText(ids["meeting-1"])
				require.NoError(err)
				assert.Contains(body, "Searchable synthetic transcript")
			})
		}
	}
}

func TestRetryDelayWaitsOutHourlyWindow(t *testing.T) {
	assert := assert.New(t)
	limited := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"3000"}}}
	assert.Equal(3000*time.Second, retryDelay(limited, 0), "a 429 waits until Omi's hourly window reopens")
	limited.Header.Set("Retry-After", "7200")
	assert.Equal(2*time.Hour, retryDelay(limited, 0))
	limited.Header.Del("Retry-After")
	assert.Equal(time.Hour, retryDelay(limited, 0), "a 429 without Retry-After waits out the whole window")
	limited.Header.Set("Retry-After", "315360000")
	assert.Equal(24*time.Hour, retryDelay(limited, 0), "an implausible Retry-After cannot block the source for years")
	limited.Header.Set("Retry-After", "soon")
	assert.Equal(time.Hour, retryDelay(limited, 3))
	unavailable := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"3000"}}}
	assert.Equal(httpretry.ProviderMaxRetryAfter, retryDelay(unavailable, 0))
}

func TestClaimPaceSlotSpacesRequestsAcrossClients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "pace")
	now := time.Now()
	at := func(ts time.Time) func() time.Time { return func() time.Time { return ts } }
	// Each claim stands in for a separate client or process.
	wait, err := claimPaceSlot(t.Context(), path, at(now))
	require.NoError(err)
	assert.Zero(wait, "an idle key is not delayed")
	for range 2 {
		// Waiting without sending does not push the next slot further out.
		wait, err = claimPaceSlot(t.Context(), path, at(now.Add(time.Second)))
		require.NoError(err)
		assert.Equal(RequestInterval-time.Second, wait)
	}
	wait, err = claimPaceSlot(t.Context(), path, at(now.Add(RequestInterval)))
	require.NoError(err)
	assert.Zero(wait)
	wait, err = claimPaceSlot(t.Context(), path, at(now.Add(-time.Hour)))
	require.NoError(err)
	assert.Zero(wait, "a clock set backward does not strand requests")
	wait, err = claimPaceSlot(t.Context(), path, at(now.Add(-time.Hour)))
	require.NoError(err)
	assert.Equal(RequestInterval, wait, "pacing resumes from the new clock")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = claimPaceSlot(ctx, path, at(now.Add(time.Hour)))
	require.ErrorIs(err, context.Canceled)
	wait, err = claimPaceSlot(t.Context(), path, at(now.Add(-time.Hour+time.Second)))
	require.NoError(err)
	assert.Equal(RequestInterval-time.Second, wait, "a canceled claim records nothing")
}

func TestImportBlankSpeechKeepsRawAndTimes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	raw := strings.Replace(fixture, `"transcript_segments":[`, `"transcript_segments":[{"text":"First speech","speaker_name":"Synthetic Speaker","start":0,"end":1},{"text":"  "},{"text":"Untimed speech","speaker_id":2,"start":10,"end":5},`, 1)
	imp := NewImporter(st, &pageSource{pages: map[int][]Conversation{0: {decodeFixture(t, raw)}}})
	sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	ids, err := st.MessageExistsBatch(sum.SourceID, []string{"meeting-1"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(ids["meeting-1"])
	require.NoError(err)
	assert.Contains(body, "[00:00] Synthetic Speaker: First speech")
	assert.Contains(body, "\nSpeaker 2: Untimed speech")
	assert.NotContains(body, "[00:00] Speaker 2")
	assert.Contains(body, "[00:00] Synthetic Speaker: Searchable synthetic transcript")
	saved, err := st.GetMessageRaw(ids["meeting-1"])
	require.NoError(err)
	assert.JSONEq(raw, string(saved))
}

type sourceFunc func(context.Context, ListParams) ([]Conversation, error)

func (f sourceFunc) ListConversations(ctx context.Context, p ListParams) ([]Conversation, error) {
	return f(ctx, p)
}

func TestClientPersistsProviderCooldownAcrossPasses(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	paceDir := t.TempDir()
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		_, err := NewPacedClient(server.URL, "omi_dev_synthetic", paceDir).ListConversations(ctx, ListParams{})
		cancel()
		var cooldown *CooldownError
		require.ErrorAs(t, err, &cooldown)
		assert.Greater(t, time.Until(cooldown.Until), time.Hour)
	}
	assert.Equal(t, 1, requests)
}

func TestImportFailedRowReplaysCurrentPage(t *testing.T) {
	for _, failure := range []string{"archive", "provider"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			first := decodeFixture(t, fixture)
			invalid := decodeFixture(t, `{"id":"meeting-2","transcript_segments":42}`)
			source := &pageSource{pages: map[int][]Conversation{0: {first, invalid}}}
			if failure == "provider" {
				source.pages[0] = []Conversation{first}
				source.failure = errors.New("provider unavailable")
			}
			imp := NewImporter(st, source)
			opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
			ctx := jobctx.WithProgress(t.Context())
			sum, err := imp.Import(ctx, opts)
			if failure == "archive" {
				require.ErrorContains(err, "unavailable transcript evidence")
				assert.Equal(int64(1), sum.MeetingsAdded)
				assert.True(jobctx.HasProgress(ctx))
			} else {
				require.ErrorContains(err, "provider unavailable")
				assert.Zero(sum.MeetingsAdded)
			}
			assert.True(sum.CheckpointSaved)
			prior, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
			require.NoError(err)
			var cursor scanCheckpoint
			require.NoError(json.Unmarshal([]byte(prior.CursorBefore.String), &cursor))
			assert.False(cursor.Ceiling.IsZero())
			_, err = st.GetLastSuccessfulSync(src.ID)
			require.ErrorIs(err, store.ErrSyncRunNotFound)
			if failure == "archive" {
				source.pages[0][1] = decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "meeting-2"))
			} else {
				source.failure = nil
			}
			source.offsets = nil
			sum, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.Equal(int64(1), sum.MeetingsAdded)
			assert.Zero(sum.MeetingsUpdated)
		})
	}
}

func TestClientPacingWaitsForPassBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		client := NewPacedClient(DefaultBaseURL, "omi_dev_synthetic", t.TempDir())
		require.NoError(client.waitTurn(t.Context(), DefaultBaseURL))
		ctx, cancel := context.WithTimeoutCause(t.Context(), time.Minute, jobctx.ErrRunBudgetExceeded)
		defer cancel()
		start := time.Now()
		err := client.waitTurn(ctx, DefaultBaseURL)
		require.ErrorIs(err, context.DeadlineExceeded)
		require.ErrorIs(context.Cause(ctx), jobctx.ErrRunBudgetExceeded)
		assert.Equal(time.Minute, time.Since(start))
	})
}

func testOmiClient(t *testing.T, url, key string) *Client {
	t.Helper()
	prior := RequestInterval
	RequestInterval = 0
	t.Cleanup(func() { RequestInterval = prior })
	normalized, err := NormalizeBaseURL(url)
	require.NoError(t, err)
	return NewPacedClient(normalized, key, t.TempDir())
}

func filterOmiPage(rows []Conversation, p ListParams) []Conversation {
	var eligible []Conversation
	for _, row := range rows {
		if (!p.CreatedBefore.IsZero() && row.CreatedAt.After(p.CreatedBefore)) || (!p.CreatedAfter.IsZero() && row.CreatedAt.Before(p.CreatedAfter)) {
			continue
		}
		eligible = append(eligible, row)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].CreatedAt.After(eligible[j].CreatedAt)
	})
	limit := p.Limit
	if limit <= 0 {
		limit = PageSize
	}
	return eligible[min(p.Offset, len(eligible)):min(p.Offset+limit, len(eligible))]
}

func omiRequest(r *http.Request) ListParams {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	upper, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end_date"))
	lower, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start_date"))
	return ListParams{Limit: limit, Offset: offset, CreatedBefore: upper, CreatedAfter: lower}
}

func serveOmiPage(w http.ResponseWriter, r *http.Request, rows []Conversation) {
	writeOmiPage(w, filterOmiPage(rows, omiRequest(r)))
}

func writeOmiPage(w http.ResponseWriter, page []Conversation) {
	raw := make([]jsontext.Value, 0, len(page))
	for _, row := range page {
		raw = append(raw, row.Raw)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		http.Error(w, "invalid fixture", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func orderedOmiRows(pages map[int][]Conversation) []Conversation {
	keys := make([]int, 0, len(pages))
	for key := range pages {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	var rows []Conversation
	for _, key := range keys {
		rows = append(rows, pages[key]...)
	}
	return rows
}

type independentTimeoutError struct{}

func (independentTimeoutError) Error() string     { return "independent HTTP timeout" }
func (independentTimeoutError) Is(err error) bool { return err == context.DeadlineExceeded }

func TestOmiPauseRequiresEveryErrorCause(t *testing.T) {
	budget, cancel := context.WithTimeoutCause(t.Context(), 0, jobctx.ErrRunBudgetExceeded)
	defer cancel()
	user, userCancel := context.WithCancel(t.Context())
	userCancel()
	yielded, yieldCancel := context.WithCancelCause(t.Context())
	yieldCancel(jobctx.ErrYieldedToWaiter)
	preempt, request := jobctx.WithPreemption(t.Context())
	request()
	cooldown := &CooldownError{Until: time.Now().Add(time.Hour)}
	storage := errors.New("synthetic checkpoint failure")
	for _, tc := range []struct {
		ctx  context.Context
		err  error
		want bool
	}{
		{t.Context(), nil, false}, {t.Context(), context.DeadlineExceeded, false}, {user, context.Canceled, false},
		{budget, budget.Err(), true}, {budget, jobctx.ErrRunBudgetExceeded, true}, {t.Context(), cooldown, true},
		{yielded, context.Canceled, true}, {preempt, context.Canceled, true},
		{budget, fmt.Errorf("wrapped: %w", errors.Join(context.DeadlineExceeded, cooldown)), true},
		{budget, errors.Join(context.DeadlineExceeded, storage), false},
		{budget, independentTimeoutError{}, false}, {budget, errors.Join(context.DeadlineExceeded, errCooldownSaveBudget), false},
	} {
		assert.Equal(t, tc.want, syncPaused(tc.ctx, tc.err), "%v", tc.err)
	}
}

func TestImportPausePreservesCheckpointAndWatermark(t *testing.T) {
	for _, reason := range []string{"cooldown", "budget", "archive"} {
		t.Run(reason, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			if reason == "archive" {
				testutil.SkipIfPostgres(t, "uses a SQLite trigger to cancel inside real conversation stats")
			}
			st := testutil.NewTestStore(t)
			if reason == "archive" {
				st.DB().SetMaxOpenConns(1)
			}
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
			rows := []Conversation{decodeFixture(t, fixture)}
			ready := sourceFunc(func(_ context.Context, p ListParams) ([]Conversation, error) {
				return filterOmiPage(rows, p), nil
			})
			_, err = NewImporter(st, ready).Import(t.Context(), opts)
			require.NoError(err)
			successful, err := st.GetLastSuccessfulSync(src.ID)
			require.NoError(err)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			client := sourceFunc(func(context.Context, ListParams) ([]Conversation, error) {
				switch reason {
				case "cooldown":
					return nil, &CooldownError{Until: time.Now().Add(time.Hour)}
				case "budget":
					cancel(jobctx.ErrRunBudgetExceeded)
					return nil, context.DeadlineExceeded
				default:
					return nil, context.Canceled
				}
			})

			entered := false
			if reason == "archive" {
				rows = append(rows, decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "paused-second")))
				conn, err := st.DB().Conn(t.Context())
				require.NoError(err)
				require.NoError(conn.Raw(func(driverConn any) error {
					sqlite, ok := driverConn.(*sqlite3.SQLiteConn)
					require.True(ok)
					return sqlite.RegisterFunc("synthetic_archive_pause", func() int { entered = true; cancel(jobctx.ErrRunBudgetExceeded); return 0 }, false)
				}))
				require.NoError(conn.Close())
				// Keep SQL active until driver cancellation interrupts the stats update.
				_, err = st.DB().Exec(`CREATE TRIGGER synthetic_archive_pause BEFORE UPDATE OF message_count ON conversations WHEN NEW.source_conversation_id = 'meeting:paused-second' BEGIN SELECT synthetic_archive_pause(); SELECT SUM(x) FROM (WITH RECURSIVE steps(x) AS (SELECT 0 UNION ALL SELECT x+1 FROM steps WHERE x < 1000000000) SELECT x FROM steps); END`)
				require.NoError(err)
				client = ready
			}
			sum, err := NewImporter(st, client).Import(ctx, opts)
			require.NoError(err)
			require.Error(sum.PauseReason)
			assert.True(sum.CheckpointSaved)
			assert.Zero(sum.Errors)
			run, err := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(err)
			assert.Equal(store.SyncStatusPaused, run.Status)
			assert.True(run.CompletedAt.Valid)
			assert.Zero(run.ErrorsCount)
			assert.Empty(run.ErrorMessage.String)
			typed, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
			require.NoError(err)
			assert.Equal(run.ID, typed.ID)
			last, err := st.GetLastSuccessfulSync(src.ID)
			require.NoError(err)
			assert.Equal(successful.ID, last.ID)
			assert.Equal(successful.CursorAfter, last.CursorAfter)
			if reason == "archive" {
				require.True(entered)
				assert.Equal(int64(1), sum.MeetingsAdded)
				assert.Equal(int64(1), run.MessagesAdded)
				_, err = st.DB().Exec(`DROP TRIGGER synthetic_archive_pause`)
				require.NoError(err)
			}
			sum, err = NewImporter(st, ready).Import(t.Context(), opts)
			require.NoError(err)
			assert.NoError(sum.PauseReason)
		})
	}
}

func TestClientDetachedCooldownSaveFailureIsNotPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		var lock *flock.Flock
		headersSent := make(chan struct{})
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(headersSent)
			if !assert.NoError(t, lock.Lock()) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Retry-After", "7200")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		client := testOmiClient(t, "http://127.0.0.1", "omi_dev_synthetic")
		client.http.Transport = server.Client().Transport
		lock = flock.New(client.pacePath(client.baseURL) + ".lock")
		defer func() { require.NoError(t, lock.Unlock()) }()
		result := make(chan error, 1)
		go func() {
			_, err := client.ListConversations(ctx, ListParams{Limit: 1})
			result <- err
		}()
		<-headersSent
		synctest.Wait()
		cancel(jobctx.ErrRunBudgetExceeded)
		require.ErrorIs(t, <-result, errCooldownSaveBudget)
	})
}

func TestPacingLockExpiryIsPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "synthetic-pacing")
		lock := flock.New(path + ".lock")
		require.NoError(t, lock.Lock())
		defer func() { require.NoError(t, lock.Unlock()) }()
		ctx, cancel := context.WithTimeoutCause(t.Context(), 100*time.Millisecond, jobctx.ErrRunBudgetExceeded)
		defer cancel()
		_, err := claimPaceSlot(ctx, path, time.Now)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestServerOutageOutlastingPassFailsSync(t *testing.T) {
	failFast := func(status int, retryAfter string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
		}
	}
	// The third request starts about 288 seconds into the pass, so a stall
	// there reaches the pass deadline before the HTTP client timeout.
	thirdStalls := func(stall http.HandlerFunc) func() http.HandlerFunc {
		return func() http.HandlerFunc {
			var requests atomic.Int64
			return func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) < 3 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				stall(w, r)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		handler func() http.HandlerFunc
		want    string
	}{
		{"internal error", func() http.HandlerFunc { return failFast(http.StatusInternalServerError, "") }, "HTTP 500"},
		{"unavailable beyond pass", func() http.HandlerFunc { return failFast(http.StatusServiceUnavailable, "3000") }, "HTTP 503"},
		{"retry stalls until deadline", thirdStalls(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}), "HTTP 500"},
		{"error body stalls until deadline", thirdStalls(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusBadGateway)
			if !assert.NoError(t, http.NewResponseController(w).Flush()) {
				return
			}
			<-r.Context().Done()
		}), "HTTP 502"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				server := httptest.NewTestServer(t, tc.handler())
				client := testOmiClient(t, "http://127.0.0.1", "omi_dev_synthetic")
				client.http.Transport = server.Client().Transport
				// Production pacing makes the outage outlast the pass before
				// retries run out.
				RequestInterval = time.Hour / TranscriptListsPerHour
				st := testutil.NewTestStore(t)
				_, err := st.GetOrCreateSource(SourceType, "work")
				require.NoError(err)
				ctx, cancel := context.WithTimeoutCause(t.Context(), 5*time.Minute, jobctx.ErrRunBudgetExceeded)
				defer cancel()

				sum, err := NewImporter(st, client).Import(ctx, ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})

				require.ErrorContains(err, tc.want)
				require.NoError(sum.PauseReason)
				assert.Equal(int64(1), sum.Errors)
				var status string
				require.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
				assert.Equal(store.SyncStatusFailed, status)
			})
		})
	}
}

func TestImportRefusesMalformedWatermarkBeforeRequests(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	id, err := st.StartSync(src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSync(id, `{"version":1,"created_after":"yesterday"}`))
	provider := &pageSource{pages: map[int][]Conversation{0: {decodeFixture(t, fixture)}}}

	_, err = NewImporter(st, provider).Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})

	require.ErrorContains(err, "decode Omi sync watermark")
	assert.Empty(t, provider.offsets, "a corrupt watermark must not start a full-history rescan")
}
