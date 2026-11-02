package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestThreadsSyncNowUsesExactScheduledAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	scheduler := newMockScheduler()
	keys := []string{"threads:123:20", "threads:123:200", "threads:456:20"}
	for _, key := range keys {
		_, err := st.GetOrCreateSource("threads", key)
		require.NoError(err)
	}
	scheduler.scheduled[keys[0]] = true
	calls := []string{}
	scheduler.triggerFn = func(key string, _ bool) error { calls = append(calls, key); return nil }
	server := NewServer(&config.Config{}, st, scheduler, testLogger())
	for n, key := range keys {
		response := servePOSTTestRequest(server, "/api/v1/sync/"+url.PathEscape(key)+"?source_type=threads")
		if n == 0 {
			assert.Equal(http.StatusAccepted, response.Code, response.Body.String())
		} else {
			assert.Equal(http.StatusNotFound, response.Code, response.Body.String())
		}
	}
	assert.Equal(keys[:1], calls)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sources/status", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var status SourceStatusResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	require.Len(status.Sources, 3)
	for _, source := range status.Sources {
		assert.Equal(source.Identifier == keys[0], source.CanSync)
	}
}
