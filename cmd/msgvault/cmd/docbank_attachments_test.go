package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/docbanktest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocbankAttachmentJobReportsUploadFailure(t *testing.T) {
	for _, fault := range []int{0, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(fault), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
			dir := t.TempDir()
			blobs, err := attachmentstore.New(store.NewPackCatalog(f.Store), dir)
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(blobs.Close()) })
			uploads := docbanktest.NewUploadHandler(t)
			for i := range 2 {
				data := fmt.Appendf(nil, "synthetic document %d", i)
				hash := fmt.Sprintf("%x", sha256.Sum256(data))
				path := filepath.Join(dir, hash[:2], hash)
				require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(os.WriteFile(path, data, 0o600))
				require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage(strconv.Itoa(i)), store.AttachmentWrite{ContentHash: hash, StoragePath: hash[:2] + "/" + hash, State: "stored"}))
				if i == 0 && fault != 0 {
					uploads.UploadFaults.Store(hash, fault)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/path" {
					_, _ = io.WriteString(w, `{"id":7,"kind":"dir"}`)
					return
				}
				uploads.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			sched := scheduler.New(nil)
			t.Cleanup(func() { <-sched.Stop().Done() })
			cfg := config.DocbankIntegrationConfig{Enabled: true, AttachmentUploadConsent: true, URL: server.URL, APIKeyEnv: beeperMediaTestKeyEnv}
			require.NoError(configureDocbankAttachmentJob(t.Context(), sched, nil, f.Store, blobs, dir, cfg, nil))
			err = sched.TriggerJob(api.DocbankAttachmentMirrorJob)
			assert.Equal(int32(2), uploads.Uploads.Load(), "one failed upload does not stop the next file")
			if fault != 0 {
				var httpErr *docbankmedia.HTTPError
				require.ErrorAs(err, &httpErr)
				assert.Equal(http.StatusServiceUnavailable, httpErr.Status)
				assert.Equal(err.Error(), sched.JobStatus()[0].LastError)
				uploads.UploadFaults.Clear()
				_, err = f.Store.DB().Exec(`UPDATE docbank_attachment_deliveries SET next_action_at=NULL WHERE state='sending'`)
				require.NoError(err)
				require.NoError(sched.TriggerJob(api.DocbankAttachmentMirrorJob))
			} else {
				require.NoError(err)
			}
			assert.Empty(sched.JobStatus()[0].LastError)
			status, err := f.Store.DocbankAttachmentStatus(t.Context(), mustDocbankAttachmentDestination(t, f.Store, cfg))
			require.NoError(err)
			assert.Equal(int64(2), status.Delivered)
		})
	}
}

func mustDocbankAttachmentDestination(t *testing.T, st *store.Store, cfg config.DocbankIntegrationConfig) string {
	t.Helper()
	destination, err := docbankAttachmentDestination(t.Context(), st, cfg)
	require.NoError(t, err)
	return destination
}

func TestDocbankAttachmentStatusCLI(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		args                    []string
		method, path            string
		status                  int
		response, want, notWant string
	}{
		{
			name: "text", args: []string{"attachments", "status"},
			method: http.MethodGet, path: "/api/v1/integrations/docbank/attachments", status: http.StatusOK,
			response: `{"state":"running","upload_consent":true,"folder":"/msgvault","counts":{"delivered":1,"sending":4,"remaining":2,"failed":3,"reasons":{"forbidden":3,"retrying":4}}}`,
			want:     "Folder: /msgvault\nDelivered: 1\nSending: 4\nFiles left to examine: 2\nFailed: 3\nforbidden: 3\nretrying: 4",
		},
		{
			name: "error_without_profile", args: []string{"attachments", "status"},
			method: http.MethodGet, path: "/api/v1/integrations/docbank/attachments", status: http.StatusOK,
			response: `{"state":"running","upload_consent":true,"folder":"/msgvault","counts":{"delivered":1,"sending":0,"remaining":0,"failed":0,"reasons":{}},"last_error":"upload unavailable"}`,
			want:     "Last error: upload unavailable", notWant: "Document profile",
		},
		{
			name: "processing", args: []string{"attachments", "status"},
			method: http.MethodGet, path: "/api/v1/integrations/docbank/attachments", status: http.StatusOK,
			response: `{"state":"running","upload_consent":true,"folder":"/msgvault","counts":{"delivered":9,"sending":0,"remaining":0,"failed":0,"reasons":{}},"processing":{"profile":"documents","waiting":3,"requested":1,"extracted":2,"outcomes":{"provider_timeout":1,"unsupported_format":2}}}`,
			want:     "Document profile: documents\nText extracted: 2\nProcessing: 1\nNot yet requested: 3\nWithout text (provider_timeout): 1\nWithout text (unsupported_format): 2",
		},
		{
			name: "no_counts", args: []string{"attachments", "status"},
			method: http.MethodGet, path: "/api/v1/integrations/docbank/attachments", status: http.StatusOK,
			response: `{"state":"running","upload_consent":false,"folder":"/msgvault"}`,
			want:     "State: running",
		},
		{
			name: "json", args: []string{"attachments", "status", "--json"},
			method: http.MethodGet, path: "/api/v1/integrations/docbank/attachments", status: http.StatusOK,
			response: `{"state":"running","upload_consent":true,"folder":"/msgvault","counts":{"delivered":1,"sending":4,"remaining":2,"failed":3,"reasons":{"forbidden":3,"retrying":4}},"last_error":"upload failed"}`,
			want:     `"last_error":"upload failed"`,
		},
		{
			name: "backfill", args: []string{"attachments", "backfill"},
			method: http.MethodPost, path: "/api/v1/integrations/docbank/attachments/backfill", status: http.StatusAccepted,
			response: `{"status":"scheduled"}`,
			want:     "Attachment mirror backfill scheduled.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(tc.method, r.Method)
				assert.Equal(tc.path, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer daemon.Close()
			command := newDocbankCmd(func(context.Context) (*daemonclient.Client, func(), error) {
				client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true})
				return client, func() {
					if client != nil {
						_ = client.Close()
					}
				}, err
			})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetArgs(tc.args)
			require.NoError(command.ExecuteContext(t.Context()))
			assert.Contains(output.String(), tc.want)
			if tc.notWant != "" {
				assert.NotContains(output.String(), tc.notWant)
			}
		})
	}
}

func TestDocbankAttachmentConsumerLifecycle(t *testing.T) {
	for _, change := range []string{"consent", "disabled", "folder", "endpoint"} {
		t.Run(change, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := storetest.New(t).Store
			sched := scheduler.New(nil)
			t.Cleanup(func() { <-sched.Stop().Done() })
			cfg := config.DocbankIntegrationConfig{Enabled: true, AttachmentUploadConsent: true, URL: "https://example.com"}
			require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, nil, t.TempDir(), t.TempDir(), cfg, nil))
			assert.True(sched.IsJobScheduled(api.DocbankAttachmentMirrorJob))
			recordings, err := newMessageRecordingReader(t.Context(), st, cfg)
			require.NoError(err)
			reader, err := newDocbankAttachmentReader(t.Context(), st, cfg, recordings.Client)
			require.NoError(err)
			require.NotNil(reader)
			destination, err := docbankAttachmentDestination(t.Context(), st, cfg)
			require.NoError(err)
			key := store.DocbankAttachmentConsumerKey(destination)
			page, err := st.NextDocbankAttachmentPage(t.Context(), destination)
			require.NoError(err)
			require.NoError(st.FinishDocbankAttachmentPage(t.Context(), destination, page))
			require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, nil, t.TempDir(), t.TempDir(), cfg, nil))
			_, err = st.GetAttachmentChangeConsumer(t.Context(), key)
			require.NoError(err)
			consumer, _, err := st.RegisterAttachmentChangeConsumer(t.Context(), "other-feature")
			require.NoError(err)
			require.NoError(st.CompleteAttachmentChangeReconciliation(t.Context(), consumer.ConsumerKey, consumer.BaselineSequence))
			switch change {
			case "consent":
				cfg.AttachmentUploadConsent = false
			case "disabled":
				cfg.Enabled = false
			case "folder":
				cfg.AttachmentFolder = "/other"
			case "endpoint":
				cfg.URL = "https://other.example.com"
			}
			gate := api.NewSerialOperationGate()
			require.NoError(configureBeeperMediaJob(t.Context(), sched, gate, st, nil, t.TempDir(), t.TempDir(), cfg, nil))
			if change == "consent" || change == "disabled" {
				reader, err = newDocbankAttachmentReader(t.Context(), nil, cfg, nil)
				require.NoError(err)
				assert.False(sched.IsJobScheduled(api.DocbankAttachmentMirrorJob))
				assert.Nil(reader)
			} else {
				assert.True(sched.IsJobScheduled(api.DocbankAttachmentMirrorJob))
			}
			_, err = st.GetAttachmentChangeConsumer(t.Context(), key)
			require.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing)
			_, err = st.GetAttachmentChangeConsumer(t.Context(), consumer.ConsumerKey)
			assert.NoError(err)
		})
	}
}

func TestDocbankAttachmentGateDrain(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	sched := scheduler.New(nil)
	t.Cleanup(func() { <-sched.Stop().Done() })
	cfg := config.DocbankIntegrationConfig{URL: "https://example.com", AttachmentUploadConsent: true}
	gate := api.NewSerialOperationGate()
	gate.StartDrain()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	err := configureBeeperMediaJob(t.Context(), sched, gate, st, nil, t.TempDir(), t.TempDir(), cfg, logger)
	require.Error(err)
	assert.NotContains(err.Error(), docbankAttachmentGateLabel)
	assert.Equal(1, strings.Count(logs.String(), "Docbank attachment mirror unavailable"))
	assert.Contains(logs.String(), "operation gate unavailable for "+docbankAttachmentGateLabel)
}
