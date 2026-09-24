package api

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
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

func TestHumaTypedRouteMarshalFailureReturns500JSON(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	srv := &Server{logger: testLogger()}
	api := srv.setupHumaAPI(mux)
	huma.Register(api, huma.Operation{
		OperationID: "testMarshalFailure",
		Method:      http.MethodGet,
		Path:        "/marshal-failure",
	}, func(context.Context, *struct{}) (*struct{ Body any }, error) {
		return &struct{ Body any }{Body: unencodableResponse{ID: 1, Bad: make(chan int)}}, nil
	})

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Length", "999")
	rec.Header().Set("Content-Encoding", "gzip")
	rec.Header().Set("Content-Disposition", "attachment")
	rec.Header().Set("ETag", `"old-body"`)
	srv.recoverMiddleware(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/marshal-failure", nil))

	assert.Equal(http.StatusInternalServerError, rec.Code)
	assert.Equal("application/json", rec.Header().Get("Content-Type"))
	assert.Empty(rec.Header().Get("Content-Length"))
	assert.Empty(rec.Header().Get("Content-Encoding"))
	assert.Empty(rec.Header().Get("Content-Disposition"))
	assert.Empty(rec.Header().Get("ETag"))
	var got ErrorResponse
	require.NoError(jsonv2.Unmarshal(rec.Body.Bytes(), &got), "body: %q", rec.Body.String())
	assert.Equal("internal_error", got.Error)
	assert.Equal("Failed to encode response", got.Message)
}

func TestHumaTypedRouteSuccessKeepsStatusAndBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	srv := &Server{logger: testLogger()}
	api := srv.setupHumaAPI(mux)
	huma.Register(api, huma.Operation{
		OperationID: "testMarshalSuccess",
		Method:      http.MethodGet,
		Path:        "/marshal-success",
	}, func(context.Context, *struct{}) (*struct{ Body any }, error) {
		return &struct{ Body any }{Body: map[string]string{"result": "ok"}}, nil
	})

	rec := httptest.NewRecorder()
	srv.recoverMiddleware(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/marshal-success", nil))

	assert.Equal(http.StatusOK, rec.Code)
	var got map[string]string
	require.NoError(jsonv2.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal("ok", got["result"])
}

func TestHumaResponseMuxFlushesBeforeHandlerReturns(t *testing.T) {
	assert := assert.New(t)
	mux := http.NewServeMux()
	rec := httptest.NewRecorder()
	humaMux := humaResponseMux{Mux: mux}
	humaMux.HandleFunc("GET /stream", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("first"))
		assert.NoError(http.NewResponseController(w).Flush())
		assert.True(rec.Flushed, "the first chunk must flush before the handler returns")
		_, _ = w.Write([]byte("second"))
	})

	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	assert.Equal(http.StatusAccepted, rec.Code)
	assert.Equal("firstsecond", rec.Body.String())
	assert.True(rec.Flushed)
}
