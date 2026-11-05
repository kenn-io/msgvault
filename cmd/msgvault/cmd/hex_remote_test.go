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
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestHexRecorderToHTTPArchiveAndMeetingContext(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

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
	must.NoError(err)
	defer func() { _ = client.Close() }()
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "you@example.com"}
	registered, err := client.ImportHex(t.Context(), hex.RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	root := t.TempDir()
	dir := filepath.Join(root, "synthetic")
	must.NoError(os.Mkdir(dir, 0700))
	must.NoError(os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema_version":1,"id":"synthetic","title":"HTTP fixture","status":"complete","created_at_ms":1790000000123,"notes":{"title":"Notes","summary":"A synthetic summary"},"calendar":{"title":"Calendar fixture","attendees":["Guest","guest@example.com"]}}`), 0600))
	must.NoError(os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":1200,\"end_ms\":2000,\"text\":\"HTTP fixture transcript\"}\n"), 0600))
	for _, track := range []string{"system", "microphone"} {
		path := filepath.Join(dir, track+".wav")
		must.NoError(os.WriteFile(path, testWAV(), 0600))
		old := time.Now().Add(-time.Minute)
		must.NoError(os.Chtimes(path, old, old))
	}
	options := hex.ImportOptions{Identifier: source.Identifier, AccountEmail: source.AccountEmail, MeetingsDir: root, LockDir: t.TempDir(), RemoteTarget: server.URL, Recordings: true}
	options.SendRecording = func(ctx context.Context, u hex.RecordingUpload, r *meetingarchive.Recording) error {
		u.NoBuildCache = true
		return client.ImportHexRecording(ctx, u, r)
	}
	send := func(ctx context.Context, r hex.RemoteRequest) (hex.RemoteResult, error) {
		r.NoBuildCache = true
		return client.ImportHex(ctx, r)
	}
	summary, err := hex.ScanRemote(t.Context(), options, send)
	must.NoError(err)
	check.Equal(int64(1), summary.MeetingsAdded)
	check.Equal(int64(2), summary.RecordingsStored)
	rows, err := st.MessageMetadataBatch(registered.SourceID, []string{"meeting:synthetic"})
	must.NoError(err)
	id := rows["meeting:synthetic"].ID
	must.Positive(id)
	msg, err := st.GetMessage(id)
	must.NoError(err)
	check.Len(msg.Attachments, 2)
	check.Equal(int64(1790000000123), msg.SentAt.UnixMilli())
	ids := []int64{id}
	include := true
	format := generated.MeetingContextRequestFormat("json")
	packet, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: &ids, IncludeTranscript: &include, Format: &format})
	must.NoError(err)
	check.Contains(packet.Content, "HTTP fixture transcript")
	check.Contains(packet.Content, "A synthetic summary")
	check.Contains(packet.Content, "guest@example.com")
	actions, err := client.ListMeetingActionItems(t.Context(), generated.ListMeetingActionItemsBody{})
	must.NoError(err)
	check.Empty(actions.Rows)
	summary, err = hex.ScanRemote(t.Context(), options, send)
	must.NoError(err)
	check.Zero(summary.MeetingsAdded + summary.MeetingsUpdated + summary.RecordingsStored)
}
