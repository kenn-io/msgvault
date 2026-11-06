package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/timeline"
)

func TestTimelineBackupFreezeDoesNotWriteEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: "peer@example.com", To: []string{storetest.OwnerAddress}, Body: "Can you send the contract?", SentAt: now.Add(-48 * time.Hour)})
	srv := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	srv.clock = func() time.Time { return now }
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.Background())) })
	token := beginBackupFreeze(t, srv)
	w := timelineGET(srv, "?date=2026-10-22", "")
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var frozen timeline.Timeline
	require.NoError(json.Unmarshal(w.Body.Bytes(), &frozen))
	require.NotNil(frozen.OpenLoops)
	require.NotEmpty(frozen.OpenLoops.Items)
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM loop_message_evidence`).Scan(&count))
	assert.Zero(count, "timeline must answer without evidence writes during the freeze")
	ended := httptest.NewRecorder()
	srv.Router().ServeHTTP(ended, endBackupFreeze(t, token))
	require.Equal(http.StatusOK, ended.Code, ended.Body.String())
	w = timelineGET(srv, "?date=2026-10-22", "")
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM loop_message_evidence`).Scan(&count))
	assert.Positive(count)
}
