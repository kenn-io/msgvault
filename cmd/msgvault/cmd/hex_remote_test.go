package cmd

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestHexRecorderToHTTPArchiveAndMeetingContext(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = false
	cfg.Server.APIKey = "synthetic-owner-key"
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: adapter.logger, OperationGate: api.NewSerialOperationGate()}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "you@example.com"}
	registered, err := client.ImportHex(t.Context(), hex.RemoteRequest{Action: "register", Source: source})
	require.NoError(t, err)
	root := t.TempDir()
	dir := filepath.Join(root, "synthetic")
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema_version":1,"id":"synthetic","title":"HTTP fixture","status":"complete","created_at_ms":1790000000123,"notes":{"title":"Notes","summary":"A synthetic summary"},"calendar":{"title":"Calendar fixture","attendees":["Guest","guest@example.com"]}}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":1200,\"end_ms\":2000,\"text\":\"HTTP fixture transcript\"}\n"), 0600))
	for _, track := range []string{"system", "microphone"} {
		path := filepath.Join(dir, track+".wav")
		require.NoError(t, os.WriteFile(path, testWAV(), 0600))
		old := time.Now().Add(-time.Minute)
		require.NoError(t, os.Chtimes(path, old, old))
	}
	options := hex.ImportOptions{Identifier: source.Identifier, AccountEmail: source.AccountEmail, MeetingsDir: root, LockDir: t.TempDir(), RemoteTarget: server.URL, Recordings: true}
	options.SendRecording = func(ctx context.Context, u hex.RecordingUpload, r *hex.Recording) error {
		u.NoBuildCache = true
		return client.ImportHexRecording(ctx, u, r)
	}
	send := func(ctx context.Context, r hex.RemoteRequest) (hex.RemoteResult, error) {
		r.NoBuildCache = true
		return client.ImportHex(ctx, r)
	}
	summary, err := hex.ScanRemote(t.Context(), options, send)
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.MeetingsAdded)
	assert.Equal(t, int64(2), summary.RecordingsStored)
	rows, err := st.MessageMetadataBatch(registered.SourceID, []string{"meeting:synthetic"})
	require.NoError(t, err)
	id := rows["meeting:synthetic"].ID
	require.Positive(t, id)
	msg, err := st.GetMessage(id)
	require.NoError(t, err)
	assert.Len(t, msg.Attachments, 2)
	assert.Equal(t, int64(1790000000123), msg.SentAt.UnixMilli())
	ids := []int64{id}
	include := true
	format := generated.MeetingContextRequestFormat("json")
	packet, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: &ids, IncludeTranscript: &include, Format: &format})
	require.NoError(t, err)
	assert.Contains(t, packet.Content, "HTTP fixture transcript")
	assert.Contains(t, packet.Content, "A synthetic summary")
	assert.Contains(t, packet.Content, "guest@example.com")
	actions, err := client.ListMeetingActionItems(t.Context(), generated.ListMeetingActionItemsBody{})
	require.NoError(t, err)
	assert.Empty(t, actions.Rows)
	summary, err = hex.ScanRemote(t.Context(), options, send)
	require.NoError(t, err)
	assert.Zero(t, summary.MeetingsAdded+summary.MeetingsUpdated+summary.RecordingsStored)
}
