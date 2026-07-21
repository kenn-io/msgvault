# Granola Date-Only Timestamps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import Granola notes when any API timestamp is an exact date-only value by interpreting it as midnight UTC.

**Architecture:** Keep Granola's exported models on `time.Time`. Add one package-local JSON timestamp type that parses RFC3339 first and exact dates second, then use focused `UnmarshalJSON` methods on the timestamp-bearing models. Give `Note` an explicit decoder so the embedded `NoteSummary` decoder cannot consume the outer note payload.

**Tech Stack:** Go 1.26, `encoding/json`, `time`, `net/http/httptest`, `testify`.

## Global Constraints

- Interpret exact `YYYY-MM-DD` values as `00:00:00Z`.
- Continue accepting RFC3339/RFC3339Nano timestamps and rejecting all other malformed values.
- Apply the fallback to note, calendar, and transcript timestamp fields.
- Preserve `Note.Raw` as the original response bytes.
- Run Go tests with `-tags "fts5 sqlite_vec"`.
- Use `assert.X` and `require.X` for all test assertions.

---

### Task 1: Decode Granola date-only timestamps at the API boundary

**Files:**
- Modify: `internal/granola/client_test.go:18-108`
- Modify: `internal/granola/models.go:8-106`

**Interfaces:**
- Consumes: Granola JSON timestamp strings in RFC3339/RFC3339Nano or exact `YYYY-MM-DD` form.
- Produces: Existing `time.Time` model fields populated with the decoded instant; no importer interface changes.

- [ ] **Step 1: Write failing client regression tests**

Add a list-path test that serves date-only summary timestamps and expects UTC midnight:

```go
func TestListNotes_AcceptsDateOnlyTimestamps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"notes":[{"id":"note-date-only","title":"Date only","owner":{"email":"user@example.com"},"created_at":"2026-05-19","updated_at":"2026-05-20"}],"hasMore":false,"cursor":null}`)
	}))
	defer srv.Close()

	out, err := NewClient(srv.URL, "grn_testkey").ListNotes(context.Background(), ListNotesParams{})
	require.NoError(err)
	require.Len(out.Notes, 1)
	assert.Equal(time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC), out.Notes[0].CreatedAt)
	assert.Equal(time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), out.Notes[0].UpdatedAt)
}
```

Add a full-note-path test that changes every timestamp-bearing section to a date-only value and verifies the response still remains available in `Raw`:

```go
func TestGetNote_AcceptsDateOnlyTimestamps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	payload := []byte(`{
		"id":"note-date-only","title":"Date only","owner":{"email":"user@example.com"},
		"created_at":"2026-05-19","updated_at":"2026-05-20",
		"calendar_event":{"scheduled_start_time":"2026-05-21","scheduled_end_time":"2026-05-22"},
		"transcript":[{"text":"Hello","start_time":"2026-05-23","end_time":"2026-05-24"}]
	}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	note, err := NewClient(srv.URL, "grn_testkey").GetNote(context.Background(), "note-date-only")
	require.NoError(err)
	assert.Equal(time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC), note.CreatedAt)
	assert.Equal(time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), note.UpdatedAt)
	require.NotNil(note.CalendarEvent)
	assert.Equal(time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC), note.CalendarEvent.ScheduledStartTime)
	assert.Equal(time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC), note.CalendarEvent.ScheduledEndTime)
	require.Len(note.Transcript, 1)
	assert.Equal(time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC), note.Transcript[0].StartTime)
	assert.Equal(time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC), note.Transcript[0].EndTime)
	assert.JSONEq(string(payload), string(note.Raw))
}
```

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/granola -run 'Test(ListNotes|GetNote)_AcceptsDateOnlyTimestamps' -count=1
```

Expected: both tests fail with a JSON time parse error because `time.Time.UnmarshalJSON` requires RFC3339.

- [ ] **Step 3: Add the minimal Granola timestamp decoder**

Add `fmt` to the `internal/granola/models.go` imports, then add the package-local
wire type and all four explicit decoders below:

```go
type apiTimestamp time.Time

func (t *apiTimestamp) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*t = apiTimestamp(time.Time{})
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode Granola timestamp: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil {
		return fmt.Errorf("parse Granola timestamp %q: %w", value, err)
	}
	*t = apiTimestamp(parsed)
	return nil
}

func (s *TranscriptSegment) UnmarshalJSON(data []byte) error {
	type plain TranscriptSegment
	decoded := struct {
		*plain
		StartTime apiTimestamp `json:"start_time"`
		EndTime   apiTimestamp `json:"end_time"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	s.StartTime = time.Time(decoded.StartTime)
	s.EndTime = time.Time(decoded.EndTime)
	return nil
}

func (e *CalendarEvent) UnmarshalJSON(data []byte) error {
	type plain CalendarEvent
	decoded := struct {
		*plain
		ScheduledStartTime apiTimestamp `json:"scheduled_start_time"`
		ScheduledEndTime   apiTimestamp `json:"scheduled_end_time"`
	}{plain: (*plain)(e)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	e.ScheduledStartTime = time.Time(decoded.ScheduledStartTime)
	e.ScheduledEndTime = time.Time(decoded.ScheduledEndTime)
	return nil
}

func (s *NoteSummary) UnmarshalJSON(data []byte) error {
	type plain NoteSummary
	decoded := struct {
		*plain
		CreatedAt apiTimestamp `json:"created_at"`
		UpdatedAt apiTimestamp `json:"updated_at"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	s.CreatedAt = time.Time(decoded.CreatedAt)
	s.UpdatedAt = time.Time(decoded.UpdatedAt)
	return nil
}

func (n *Note) UnmarshalJSON(data []byte) error {
	var summary NoteSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return err
	}
	var fields struct {
		WebURL           string              `json:"web_url"`
		CalendarEvent    *CalendarEvent      `json:"calendar_event"`
		Attendees        []User              `json:"attendees"`
		FolderMembership []Folder            `json:"folder_membership"`
		SummaryText      string              `json:"summary_text"`
		SummaryMarkdown  string              `json:"summary_markdown"`
		Transcript       []TranscriptSegment `json:"transcript"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*n = Note{
		NoteSummary:      summary,
		WebURL:           fields.WebURL,
		CalendarEvent:    fields.CalendarEvent,
		Attendees:        fields.Attendees,
		FolderMembership: fields.FolderMembership,
		SummaryText:      fields.SummaryText,
		SummaryMarkdown:  fields.SummaryMarkdown,
		Transcript:       fields.Transcript,
	}
	return nil
}
```

The explicit `Note` decoder combines the embedded summary with the remaining
fields so `NoteSummary.UnmarshalJSON` cannot be promoted as the decoder for the
entire outer note.

- [ ] **Step 4: Format and verify GREEN**

Run:

```bash
go fmt ./...
go test -tags "fts5 sqlite_vec" ./internal/granola -run 'Test(ListNotes|GetNote)_AcceptsDateOnlyTimestamps' -count=1
go test -tags "fts5 sqlite_vec" ./internal/granola -count=1
go vet -tags "fts5 sqlite_vec" ./internal/granola
```

Expected: every command exits 0.

- [ ] **Step 5: Run repository verification**

Run:

```bash
make test
go fmt ./...
go vet ./...
git diff --check
```

Expected: every command exits 0 and no tracked file outside the Granola fix or its approved docs changes unexpectedly.

- [ ] **Step 6: Commit the implementation**

```bash
git add internal/granola/models.go internal/granola/client_test.go
git commit -m "fix: accept Granola date-only timestamps"
```

The commit body must explain that one bare API date previously prevented an entire note from being archived, why midnight UTC is deterministic, and which verification commands passed. Use the mandatory commit skill and do not bypass hooks.
