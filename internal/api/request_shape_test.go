package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

func TestRequestLogsSanitizedSearchShape(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		buf := &syncBuffer{}
		server := &Server{logger: slog.New(slog.NewJSONHandler(buf, nil)), inProgressThreshold: time.Second}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/search/fast?q="+url.QueryEscape(`from:reader@example.test after:2024-01-01 "secret phrase" unknown:secret`)+"&limit=2&source_id=913&private_key=secret", nil)
		request.RemoteAddr = "192.0.2.1:4321"
		request.Header.Set("X-Forwarded-For", "198.51.100.99")
		handler := server.loggerMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			synctest.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), request)
		for _, message := range []string{"http request in progress", "http request"} {
			record := findJSONLogLine(t, buf.String(), message)
			assert.Equal(t, "192.0.2.1:4321", record["remote_addr"])
			assert.Equal(t, []any{"after", "from", "limit", "source_id", "text"}, record["query_shape"])
		}
		for _, value := range []string{"reader@example.test", "2024-01-01", "secret", "913", "198.51.100.99", "private_key"} {
			assert.NotContains(t, buf.String(), value)
		}
	})
}

func FuzzRequestQueryShapeVocabulary(f *testing.F) {
	for _, seed := range []string{"", `from:reader@example.test subject:"secret value"`, `before:invalid`, "unknown:secret", "☃\x00\xff", `has:attachment list:alerts.example.test in:7 message_type:sms`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		buf := &syncBuffer{}
		server := &Server{logger: slog.New(slog.NewJSONHandler(buf, nil))}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/search/fast?q="+url.QueryEscape(input), nil)
		server.loggerMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), request)
		record := findJSONLogLine(t, buf.String(), "http request")
		shape, ok := record["query_shape"].([]any)
		require.True(t, ok)
		allowed := strings.Fields("text from to cc bcc subject label list account received has before after larger smaller in conversation_id message_type")
		for _, operator := range shape {
			assert.Contains(t, allowed, operator)
		}
	})
}

func TestFastSearchFailureLogOmitsQueryValues(t *testing.T) {
	assert := assert.New(t)
	buf := &syncBuffer{}
	engine := &querytest.MockEngine{SearchFastWithStatsFunc: func(context.Context, *search.Query, string, query.MessageFilter, query.ViewType, int, int) (*query.SearchFastResult, error) {
		return nil, errors.New("synthetic failure")
	}}
	server := newTestServerWithEngine(t, engine)
	server.logger = slog.New(slog.NewJSONHandler(buf, nil))
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/search/fast?q=subject:secret-value", nil))
	require.Equal(t, http.StatusInternalServerError, response.Code)
	record := findJSONLogLine(t, buf.String(), "fast search failed")
	assert.NotContains(record, "query")
	assert.Equal([]any{"subject"}, record["query_shape"])
	assert.NotContains(buf.String(), "secret-value")
}
