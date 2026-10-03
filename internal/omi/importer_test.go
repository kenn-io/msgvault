package omi

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/time/rate"
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
	older := strings.ReplaceAll(fixture, "meeting-1", "older")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "0":
			_, _ = fmt.Fprintf(w, "[%s]", rawSummary)
		case "200":
			_, _ = fmt.Fprintf(w, "[%s]", older)
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "omi_dev_synthetic")
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	imp := NewImporter(st, client)
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(2), sum.MeetingsAdded)
	ids, err := st.MessageExistsBatch(src.ID, []string{"summary-only", "older"})
	require.NoError(err)
	require.Len(ids, 2)
	raw, err := st.GetMessageRaw(ids["summary-only"])
	require.NoError(err)
	assert.JSONEq(rawSummary, string(raw))
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Equal(meetingcontent.StateUnavailable, content.Transcript.State)
	assert.Equal(meetingcontent.StateUnsupported, content.Notes.State)
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
			for _, replacement := range []string{"null", "missing", "reversed"} {
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
						offset, parseErr := strconv.Atoi(r.URL.Query().Get("offset"))
						if parseErr != nil {
							http.Error(w, "invalid offset", http.StatusBadRequest)
							return
						}
						items := []jsontext.Value{}
						for _, c := range pages[offset] {
							items = append(items, c.Raw)
						}
						data, marshalErr := json.Marshal(items)
						if marshalErr != nil {
							http.Error(w, "invalid fixture page", http.StatusInternalServerError)
							return
						}
						_, _ = w.Write(data)
					}))
					defer server.Close()
					client := NewClient(server.URL, "omi_dev_synthetic")
					client.limiter = rate.NewLimiter(rate.Inf, 1)
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
					case "reversed":
						fields["transcript_segments"] = jsontext.Value(`[{"text":"Badreplacement","start":10,"end":5}]`)
					}
					replaced, err := json.Marshal(fields)
					require.NoError(err)
					pages[0] = []Conversation{decodeFixture(t, string(replaced)), decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "same-page"))}
					pages[PageSize] = []Conversation{decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "older-page"))}
					sum, err := imp.Import(context.Background(), opts)
					if replacement == "reversed" {
						require.ErrorContains(err, "unavailable transcript evidence")
						assert.Equal(int64(1), sum.Errors)
					} else {
						require.NoError(err, "keeping an archived transcript is not a failure")
						assert.Zero(sum.Errors)
					}
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
					if replacement == "reversed" {
						assert.Equal("failed", latest.Status)
						assert.Equal(int64(1), latest.ErrorsCount)
					} else {
						assert.Equal("completed", latest.Status)
					}
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
	if p.Offset > 0 && s.failure != nil {
		return nil, s.failure
	}
	return s.pages[p.Offset], nil
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
	assert.Equal([]int{0, 200}, source.offsets, "short filtered pages must not terminate traversal")
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
		assert.Empty(r.URL.Query().Get("start_date"), "creation lower bounds are applied locally")
		assert.NotEmpty(r.URL.Query().Get("end_date"), "the fixed import boundary stabilizes pagination")
		offset, parseErr := strconv.Atoi(r.URL.Query().Get("offset"))
		if parseErr != nil {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		offsets = append(offsets, offset)
		items := []jsontext.Value{}
		for _, c := range pages[offset] {
			items = append(items, c.Raw)
		}
		data, marshalErr := json.Marshal(items)
		if marshalErr != nil {
			http.Error(w, "invalid fixture page", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	client := NewClient(server.URL, "omi_dev_synthetic")
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	imp := NewImporter(st, client)
	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "work", AccountEmail: "owner@example.com", CreatedAfter: after,
	})
	require.NoError(err)
	assert.Equal(int64(2), sum.MeetingsAdded)
	assert.Equal([]int{0}, offsets, "descending creation-time pages stop at the inclusive cutoff")
	ids, err := st.MessageExistsBatch(src.ID, []string{"late-created", "at-cutoff", "old-created"})
	require.NoError(err)
	assert.Contains(ids, "late-created", "a recent creation is included even when the meeting started earlier")
	assert.Contains(ids, "at-cutoff", "the creation-date bound is inclusive")
	assert.NotContains(ids, "old-created", "an old creation is excluded even when its meeting started later")
}

func TestImportPartialFailureAndRepeatedPage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	source := &pageSource{pages: map[int][]Conversation{0: {decodeFixture(t, fixture)}}, failure: errors.New("provider unavailable")}
	imp := NewImporter(st, source)
	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
	require.ErrorContains(err, "provider unavailable")
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.Equal(int64(1), sum.Errors)
	_, err = st.GetLastSuccessfulSync(sum.SourceID)
	require.Error(err)
	source.failure = nil
	source.pages[PageSize] = source.pages[0]
	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
	require.ErrorContains(err, "pagination repeated")
	delete(source.pages, PageSize)
	repaired, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
	require.NoError(err)
	assert.Zero(repaired.MeetingsAdded + repaired.MeetingsUpdated)
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
				assert.Empty(r.URL.Query().Get("start_date"), "creation lower bounds are applied locally")
				assert.Equal("2026-01-02T00:00:00Z", r.URL.Query().Get("end_date"))
				_, _ = w.Write([]byte("[" + fixture + "]"))
			}))
			if tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			client := NewClient(server.URL+"/prefix/", "omi_dev_synthetic")
			client.http = server.Client()
			result, err := client.ListConversations(context.Background(), ListParams{
				Limit: 1000, Offset: 200,
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
			_, err := NewClient(server.URL, "test").ListConversations(context.Background(), ListParams{})
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
	client := NewClient(server.URL, "test")
	client.limiter = rate.NewLimiter(rate.Inf, 1)
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
	next := decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "meeting-2"))
	source := &pageSource{pages: map[int][]Conversation{0: {completed, next}}}
	imp := NewImporter(st, source)
	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Limit: 1})
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.Equal([]int{0}, source.offsets)
	invalid := decodeFixture(t, `{"id":"meeting-1","structured":{"title":"Invalid replacement"}}`)
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

func TestImportLimitBoundsPageAndAdvancesByRequestedSize(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	first := decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "first"))
	second := decodeFixture(t, strings.ReplaceAll(fixture, "meeting-1", "second"))
	pages := map[int][]Conversation{0: {first}, 2: {second}}
	var limits []int
	var offsets []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, limitErr := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, offsetErr := strconv.Atoi(r.URL.Query().Get("offset"))
		if limitErr != nil || offsetErr != nil {
			http.Error(w, "invalid pagination", http.StatusBadRequest)
			return
		}
		limits = append(limits, limit)
		offsets = append(offsets, offset)
		items := []jsontext.Value{}
		for _, c := range pages[offset] {
			items = append(items, c.Raw)
		}
		data, marshalErr := json.Marshal(items)
		if marshalErr != nil {
			http.Error(w, "invalid fixture page", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	client := NewClient(server.URL, "omi_dev_synthetic")
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	imp := NewImporter(st, client)
	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "work", AccountEmail: "owner@example.com", Limit: 2,
	})
	require.NoError(err)
	assert.Equal(int64(2), sum.MeetingsProcessed)
	assert.Equal([]int{2, 1}, limits, "requests shrink to the remaining processing limit")
	assert.Equal([]int{0, 2}, offsets, "offsets advance by each requested page size")
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
	var offsets []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.URL.Query().Get("start_date"), "creation lower bounds are applied locally")
		offset, offsetErr := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, limitErr := strconv.Atoi(r.URL.Query().Get("limit"))
		if err := errors.Join(offsetErr, limitErr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		offsets = append(offsets, offset)
		items := []jsontext.Value{}
		for _, c := range history {
			items = append(items, c.Raw)
		}
		items = items[min(offset, len(items)):]
		data, marshalErr := json.Marshal(items[:min(limit, len(items))])
		if marshalErr != nil {
			http.Error(w, "invalid fixture page", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	client := NewClient(server.URL, "omi_dev_synthetic")
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	imp := NewImporter(st, client)
	run := func(opts ImportOptions) (int64, string) {
		t.Helper()
		offsets = nil
		opts.Identifier, opts.AccountEmail = "work", "owner@example.com"
		sum, err := imp.Import(context.Background(), opts)
		require.NoError(err)
		last, err := st.GetLastSuccessfulSync(src.ID)
		require.NoError(err)
		return sum.MeetingsProcessed, last.CursorAfter.String
	}

	processed, cursor := run(ImportOptions{})
	assert.Equal(int64(2), processed, "the first sync reads all history")
	assert.Equal([]int{0, PageSize}, offsets)
	assert.JSONEq(`{"created_after":"2026-01-01T12:00:00Z"}`, cursor)

	history = append([]Conversation{conversation("new", day(5))}, history...)
	processed, cursor = run(ImportOptions{})
	assert.Equal(int64(2), processed, "later syncs stop 48 hours before the watermark")
	assert.Equal([]int{0}, offsets, "crossing the overlap ends the scan without another request")
	assert.JSONEq(`{"created_after":"2026-01-05T12:00:00Z"}`, cursor)

	history = append([]Conversation{conversation("newest", day(10))}, history...)
	processed, cursor = run(ImportOptions{Limit: 1})
	assert.Equal(int64(1), processed)
	assert.JSONEq(`{"created_after":"2026-01-05T12:00:00Z"}`, cursor, "a limited run keeps the watermark")

	processed, cursor = run(ImportOptions{Full: true})
	assert.Equal(int64(4), processed, "a full sync rescans all history")
	assert.JSONEq(`{"created_after":"2026-01-10T12:00:00Z"}`, cursor)
	ids, err := st.MessageExistsBatch(src.ID, []string{"ancient", "old", "new", "newest"})
	require.NoError(err)
	assert.Len(ids, 4)
}

func TestClientPacesTranscriptListsToHourlyBudget(t *testing.T) {
	assert := assert.New(t)
	limiter := NewClient(DefaultBaseURL, "omi_dev_synthetic").limiter
	now := time.Now()
	assert.Zero(limiter.ReserveN(now, 1).DelayFrom(now))
	for i := 1; i <= TranscriptListsPerHour; i++ {
		assert.Equal(time.Duration(i)*time.Hour/TranscriptListsPerHour, limiter.ReserveN(now, 1).DelayFrom(now), "requests are spaced evenly, so no hour holds a 26th")
	}
}

func TestRetryDelayWaitsOutHourlyWindow(t *testing.T) {
	assert := assert.New(t)
	limited := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"3000"}}}
	assert.Equal(3000*time.Second, retryDelay(limited, 0), "a 429 waits until Omi's hourly window reopens")
	limited.Header.Set("Retry-After", "7200")
	assert.Equal(time.Hour, retryDelay(limited, 0))
	limited.Header.Del("Retry-After")
	assert.Equal(time.Hour, retryDelay(limited, 0), "a 429 without Retry-After waits out the whole window")
	limited.Header.Set("Retry-After", "soon")
	assert.Equal(time.Hour, retryDelay(limited, 3))
	unavailable := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"3000"}}}
	assert.Equal(httpretry.ProviderMaxRetryAfter, retryDelay(unavailable, 0))
}
