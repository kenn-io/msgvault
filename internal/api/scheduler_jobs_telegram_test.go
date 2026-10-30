package api

import (
	"net/http"
	"testing"

	"go.kenn.io/msgvault/internal/config"

	"github.com/stretchr/testify/assert"
)

func TestTelegramSyncNowTriggersScheduledAccount(t *testing.T) {
	assert := assert.New(t)
	sched := newMockScheduler()
	sched.scheduled["user999"] = true
	var triggered []string
	sched.triggerFn = func(account string, _ bool) error {
		triggered = append(triggered, account)
		return nil
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, nil, sched, testLogger())
	response := servePOSTTestRequest(srv, "/api/v1/sync/user999?source_type=telegram")
	assert.Equal(http.StatusAccepted, response.Code, response.Body.String())
	assert.Equal([]string{"user999"}, triggered)

	sched.scheduled["user999"] = false
	response = servePOSTTestRequest(srv, "/api/v1/sync/user999?source_type=telegram")
	assert.Equal(http.StatusNotFound, response.Code, response.Body.String())
	assert.Equal([]string{"user999"}, triggered)
}
