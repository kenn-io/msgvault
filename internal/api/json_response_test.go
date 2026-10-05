package api

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type unencodableResponse struct {
	ID  int64    `json:"id"`
	Bad chan int `json:"bad"`
}

func TestMarshalAPIJSONWritesNothingOnError(t *testing.T) {
	var buf bytes.Buffer
	err := marshalAPIJSON(&buf, unencodableResponse{ID: 1, Bad: make(chan int)})
	require.Error(t, err)
	assert.Empty(t, buf.String(), "a failed marshal must not leave a partial body")
}

func TestWriteJSONMarshalFailureReturns500WithBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rec := httptest.NewRecorder()

	writeJSON(rec, http.StatusOK, unencodableResponse{ID: 1, Bad: make(chan int)})

	assert.Equal(http.StatusInternalServerError, rec.Code)
	var got ErrorResponse
	require.NoError(jsonv2.Unmarshal(rec.Body.Bytes(), &got), "body: %q", rec.Body.String())
	assert.Equal("internal_error", got.Error)
	assert.Equal("Failed to encode response", got.Message)
}

const truncatedEmojiSnippet = "Calendar: lunch \xf0\x9f" // an emoji cut after two bytes

func TestWriteJSONReplacesInvalidUTF8(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rec := httptest.NewRecorder()

	writeJSON(rec, http.StatusOK, cliMessageResponse{ID: 7, Subject: "ok", Snippet: truncatedEmojiSnippet, BodyText: "tail"})

	assert.Equal(http.StatusOK, rec.Code)
	assert.Equal("nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.True(jsontext.Value(rec.Body.Bytes()).IsValid(), "body: %q", rec.Body.String())
	var got cliMessageResponse
	require.NoError(jsonv2.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(int64(7), got.ID)
	assert.Equal("Calendar: lunch ��", got.Snippet)
	assert.Equal("tail", got.BodyText, "fields after the bad one must be present")
}

type ndjsonTestEvent struct {
	Line string `json:"line"`
}

func TestCLINDJSONEventWriterReplacesInvalidUTF8(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rec := httptest.NewRecorder()
	write := newCLINDJSONEventWriter[ndjsonTestEvent](rec)

	require.NoError(write(ndjsonTestEvent{Line: truncatedEmojiSnippet}))
	require.NoError(write(ndjsonTestEvent{Line: "next"}))

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	require.Len(lines, 2)
	for _, line := range lines {
		assert.True(jsontext.Value(line).IsValid(), "line: %q", line)
	}
	var first ndjsonTestEvent
	require.NoError(jsonv2.Unmarshal([]byte(lines[0]), &first))
	assert.Equal("Calendar: lunch ��", first.Line)
	assert.Contains(lines[1], `"next"`)
}

// The HTTP transport reports flush failures through FlushError; middleware must preserve them.
type failingFlushResponse struct {
	*httptest.ResponseRecorder

	err error
}

func (w *failingFlushResponse) FlushError() error { return w.err }
func TestCLINDJSONEventWriterPropagatesFlushFailureThroughMiddleware(t *testing.T) {
	transportErr := errors.New("synthetic connection closed")
	raw := &failingFlushResponse{httptest.NewRecorder(), transportErr}
	wrapped := newTrackingResponseWriter(newTrackingResponseWriter(raw))
	write := newCLINDJSONEventWriter[ndjsonTestEvent](wrapped)
	require.ErrorIs(t, write(ndjsonTestEvent{Line: "progress"}), transportErr)
	assert.Equal(t, http.StatusOK, wrapped.Status())
}
