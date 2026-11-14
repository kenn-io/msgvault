package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChunkDownloadsThroughDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	engine := query.NewSQLiteEngine(st.DB())
	cfg := &config.Config{}
	cfg.Data.DataDir = t.TempDir()
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	must.NoError(err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "download-thread", "Download")
	must.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "download-message", MessageType: "email"})
	must.NoError(err)
	var originalReads, attachmentReads atomic.Int64
	router := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)}).Router()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/message/original":
			checks.Equal("268435456", r.URL.Query().Get("max_bytes"), "bound the daemon read before allocating original MIME")
			originalReads.Add(1)
		case "/api/v1/cli/attachment":
			attachmentReads.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(client.Close()) })
	server := newMCPHTTPServer(ServeOptions{Engine: daemonclient.NewEngineAdapter(client), AttachmentReader: client}, HTTPOptions{})
	t.Cleanup(func() { checks.NoError(server.Shutdown(context.WithoutCancel(t.Context()))) })
	chunk := func(t *testing.T, name string, args map[string]any) exportChunkResp {
		t.Helper()
		must := require.New(t)
		response, raw := task4RawRequest(t, server.Handler, "tools/call", map[string]any{"name": name, "arguments": args}, nil)
		must.Empty(response.Error, "%s", raw)
		must.NotEqual(true, response.Result["isError"], "%s", raw)
		encoded, err := json.Marshal(response.Result["structuredContent"])
		must.NoError(err)
		var result exportChunkResp
		must.NoError(json.Unmarshal(encoded, &result))
		return result
	}

	t.Run("original snapshot survives raw replacement", func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		data := bytes.Repeat([]byte("original"), (40<<20)/8)
		must.NoError(st.UpsertMessageRaw(id, data))
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		for offset := 0; offset < len(data); offset += 1 << 20 {
			args := map[string]any{"id": id, "offset": offset, "length": 1 << 20}
			if offset > 0 {
				args["sha256"] = digest
			}
			got := chunk(t, ToolExportEML, args)
			part, err := base64.StdEncoding.DecodeString(got.DataBase64)
			must.NoError(err)
			must.Equal(data[offset:offset+(1<<20)], part)
			checks.Equal(digest, got.SHA256)
			checks.Equal(offset+(1<<20) == len(data), got.Complete)
			if offset == 0 {
				must.NoError(st.UpsertMessageRaw(id, []byte("replacement MIME")))
				response, raw := task4RawRequest(t, server.Handler, "tools/call", map[string]any{
					"name":      ToolExportEML,
					"arguments": map[string]any{"id": id, "offset": 1, "sha256": fmt.Sprintf("%064x", 0)},
				}, nil)
				must.Empty(response.Error)
				checks.Equal(true, response.Result["isError"])
				checks.Contains(raw, "restart at offset 0")
			}
		}
		checks.Equal(int64(1), originalReads.Load())
	})

	t.Run("large attachment chunks retain full response cap", func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		data := bytes.Repeat([]byte("attachment"), (80<<20)/10+1)[:80<<20]
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		path, err := msgexport.StoragePath(cfg.AttachmentsDir(), digest)
		must.NoError(err)
		must.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
		must.NoError(os.WriteFile(path, data, 0o600))
		must.NoError(st.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{Filename: "large.bin", MIMEType: "application/octet-stream", ContentHash: digest, StoragePath: path, Size: int64(len(data))}))
		attachments, err := engine.GetAttachmentsByHash(t.Context(), digest)
		must.NoError(err)
		must.Len(attachments, 1)
		attachmentID := attachments[0].ID
		for offset := 0; offset < 2<<20; offset += 1 << 20 {
			got := chunk(t, ToolGetAttachment, map[string]any{"attachment_id": attachmentID, "offset": offset, "length": 1 << 20, "sha256": digest})
			part, err := base64.StdEncoding.DecodeString(got.DataBase64)
			must.NoError(err)
			checks.Equal(data[offset:offset+(1<<20)], part)
			checks.Equal(int64(80<<20), got.Size)
			checks.Equal(digest, got.SHA256)
		}
		checks.Equal(int64(1), attachmentReads.Load())
		response, raw := task4RawRequest(t, server.Handler, "tools/call", map[string]any{"name": ToolGetAttachment, "arguments": map[string]any{"attachment_id": attachmentID}}, nil)
		must.Empty(response.Error)
		checks.Equal(true, response.Result["isError"])
		checks.Contains(raw, "attachment too large: 83886080 bytes (max 52428800)")
		response, _ = task4RawRequest(t, server.Handler, "resources/read", map[string]any{"uri": attachmentResourceURI(attachmentID)}, nil)
		checks.NotEmpty(response.Error)
	})
}

func TestDownloadSnapshotExpiryAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		var cache downloadCache
		defer cache.close()
		key := downloadKey{attachment: 1}
		load := func() (*downloadSnapshot, error) {
			return &downloadSnapshot{data: []byte("first version")}, nil
		}
		first, err := cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err)
		for _, validationErr := range []error{context.Canceled, errors.New("synthetic authorization failure")} {
			first.validate = func(context.Context) error { return validationErr }
			_, err = cache.get(t.Context(), key, chunkRequest{}, load)
			must.ErrorIs(err, validationErr)
			checks.Same(first, cache.entries[key])
			checks.Equal(len(first.data), cache.bytes)
		}
		first.validate = nil
		_, err = cache.get(t.Context(), key, chunkRequest{digest: "wrong"}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Same(first, cache.entries[key])
		first.validate = func(context.Context) error { return errDownloadExpired }
		_, err = cache.get(t.Context(), key, chunkRequest{offset: 1, digest: first.digest}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Zero(cache.bytes)
		first, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err, "invalidated entry can restart immediately")
		first.validate = func(context.Context) error { return fmt.Errorf("snapshot changed: %w", errDownloadExpired) }
		load = func() (*downloadSnapshot, error) { return &downloadSnapshot{data: []byte("updated bytes")}, nil }
		first, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err, "offset zero also replaces an invalidated cached entry directly")
		checks.Equal([]byte("updated bytes"), first.data)
		checks.Equal(len(first.data), cache.bytes)
		time.Sleep(downloadLifetime)
		synctest.Wait()
		_, err = cache.get(t.Context(), key, chunkRequest{offset: 1, digest: first.digest}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Zero(cache.bytes, "expiry releases the retained download budget")
		_, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err, "offset zero can start a new download")
		cache.close()
		_, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Zero(cache.bytes, "shutdown releases snapshots without waiting for expiry")
	})
}

func TestDownloadSnapshotEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		var cache downloadCache
		defer cache.close()
		load := func() (*downloadSnapshot, error) {
			return &downloadSnapshot{data: []byte("a")}, nil
		}
		var first *downloadSnapshot
		for i := int64(1); i <= maxDownloads+1; i++ {
			snapshot, err := cache.get(t.Context(), downloadKey{attachment: i}, chunkRequest{}, load)
			must.NoError(err)
			if i == 1 {
				first = snapshot
			}
			// Distinct virtual timestamps identify the oldest snapshot on every OS.
			time.Sleep(time.Second)
		}
		_, err := cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{offset: 1, digest: first.digest}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Equal(maxDownloads, cache.bytes)
	})
}

func TestDownloadSnapshotByteBudget(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	var cache downloadCache
	defer cache.close()
	data := make([]byte, MaxDownloadBytes/2+1)
	load := func() (*downloadSnapshot, error) {
		return &downloadSnapshot{data: data}, nil
	}
	first, err := cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{}, load)
	must.NoError(err)
	_, err = cache.get(t.Context(), downloadKey{attachment: 2}, chunkRequest{}, load)
	must.NoError(err)
	_, err = cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{offset: 1, digest: first.digest}, load)
	must.ErrorIs(err, errDownloadExpired, "byte budget evicts before the entry limit")
	checks.Equal(len(data), cache.bytes)
}

func TestDownloadSnapshotSlowLoadDoesNotBlockOtherDownloads(t *testing.T) {
	var cache downloadCache
	defer cache.close()
	load := func() (*downloadSnapshot, error) { return &downloadSnapshot{data: []byte("cached")}, nil }
	key := downloadKey{attachment: 1}
	first, err := cache.get(t.Context(), key, chunkRequest{}, load)
	require.NoError(t, err)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.get(t.Context(), downloadKey{operation: "get_person_record", identity: "slow"}, chunkRequest{}, func() (*downloadSnapshot, error) {
			close(started)
			<-release
			return &downloadSnapshot{data: []byte("slow")}, nil
		})
	}()
	<-started
	result := make(chan error, 1)
	go func() {
		_, err := cache.get(t.Context(), key, chunkRequest{offset: 1, digest: first.digest}, load)
		result <- err
	}()
	select {
	case err := <-result:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		assert.Fail(t, "cached download waited for an unrelated load")
		close(release)
		<-result
		<-done
		return
	}
	close(release)
	<-done
}

func TestDownloadSnapshotCloseDuringLoad(t *testing.T) {
	var cache downloadCache
	started, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{}, func() (*downloadSnapshot, error) {
			close(started)
			<-release
			return &downloadSnapshot{data: []byte("slow")}, nil
		})
		result <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() { cache.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		assert.Fail(t, "closing cache waited for the slow load")
		close(release)
		<-closed
		<-result
		return
	}
	close(release)
	require.ErrorIs(t, <-result, errDownloadExpired)
}

func TestDownloadSnapshotConcurrentRequestsShareLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache downloadCache
		defer cache.close()
		started, release := make(chan struct{}), make(chan struct{})
		var reads atomic.Int64
		load := func() (*downloadSnapshot, error) {
			if reads.Add(1) == 1 {
				close(started)
			}
			<-release
			return &downloadSnapshot{data: []byte("snapshot")}, nil
		}
		results := make(chan error, 4)
		for range 4 {
			go func() {
				_, err := cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{}, load)
				results <- err
			}()
		}
		<-started
		synctest.Wait()
		assert.Equal(t, int64(1), reads.Load())
		close(release)
		for range 4 {
			require.NoError(t, <-results)
		}
	})
}

func TestDownloadSnapshotWaiterCancellationAndLeaderRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache downloadCache
		defer cache.close()
		key := downloadKey{attachment: 1}
		leaderCtx, cancelLeader := context.WithCancel(t.Context())
		waiterCtx, cancelWaiter := context.WithCancel(t.Context())
		defer cancelLeader()
		defer cancelWaiter()
		started := make(chan struct{})
		leader := make(chan error, 1)
		go func() {
			_, err := cache.get(leaderCtx, key, chunkRequest{}, func() (*downloadSnapshot, error) {
				close(started)
				<-leaderCtx.Done()
				return nil, leaderCtx.Err()
			})
			leader <- err
		}()
		<-started
		canceled := make(chan error, 1)
		go func() {
			_, err := cache.get(waiterCtx, key, chunkRequest{}, func() (*downloadSnapshot, error) {
				return nil, errors.New("canceled waiter loaded")
			})
			canceled <- err
		}()
		live := make(chan error, 1)
		go func() {
			_, err := cache.get(t.Context(), key, chunkRequest{}, func() (*downloadSnapshot, error) {
				return &downloadSnapshot{data: []byte("retry")}, nil
			})
			live <- err
		}()
		synctest.Wait()
		cancelWaiter()
		require.ErrorIs(t, <-canceled, context.Canceled)
		cancelLeader()
		require.ErrorIs(t, <-leader, context.Canceled)
		require.NoError(t, <-live)
	})
}

func TestPersonDownloadRefreshDoesNotShareLoad(t *testing.T) {
	var cache downloadCache
	defer cache.close()
	key := downloadKey{operation: "get_person_record", identity: "1"}
	started, release := make(chan struct{}), make(chan struct{})
	old := make(chan error, 1)
	go func() {
		_, err := cache.get(t.Context(), key, chunkRequest{length: 1}, func() (*downloadSnapshot, error) {
			close(started)
			<-release
			return &downloadSnapshot{data: []byte("old")}, nil
		})
		old <- err
	}()
	<-started
	type result struct {
		snapshot *downloadSnapshot
		err      error
	}
	updated := make(chan result, 1)
	go func() {
		current, err := cache.get(t.Context(), key, chunkRequest{length: 1}, func() (*downloadSnapshot, error) {
			return &downloadSnapshot{data: []byte("new")}, nil
		})
		updated <- result{current, err}
	}()
	var current result
	select {
	case current = <-updated:
	case <-time.After(time.Second):
		assert.Fail(t, "profile refresh waited for an earlier load")
		close(release)
		<-updated
		<-old
		return
	}
	close(release)
	require.NoError(t, <-old)
	require.NoError(t, current.err)
	assert.Equal(t, []byte("new"), current.snapshot.data)
	continued, err := cache.get(t.Context(), key, chunkRequest{offset: 1, digest: current.snapshot.digest}, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), continued.data)
}

func TestPersonDownloadFailedRefreshKeepsEarlierLoad(t *testing.T) {
	var cache downloadCache
	defer cache.close()
	key := downloadKey{operation: "get_person_record", identity: "1"}
	started, release := make(chan struct{}), make(chan struct{})
	result := make(chan *downloadSnapshot, 1)
	go func() {
		snapshot, _ := cache.get(t.Context(), key, chunkRequest{length: 1}, func() (*downloadSnapshot, error) {
			close(started)
			<-release
			return &downloadSnapshot{data: []byte("first")}, nil
		})
		result <- snapshot
	}()
	<-started
	loadErr := errors.New("refresh failed")
	_, err := cache.get(t.Context(), key, chunkRequest{length: 1}, func() (*downloadSnapshot, error) {
		return nil, loadErr
	})
	close(release)
	first := <-result
	require.ErrorIs(t, err, loadErr)
	require.NotNil(t, first)
	continued, err := cache.get(t.Context(), key, chunkRequest{offset: 1, digest: first.digest}, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), continued.data)
}

func TestPersonDownloadSnapshotReuseRefreshAndSmallResults(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	var cache downloadCache
	defer cache.close()
	key := downloadKey{operation: "get_person_record", identity: "1"}
	value, reads := "first", 0
	load := func() (*downloadSnapshot, error) {
		reads++
		return &downloadSnapshot{data: []byte(value), etag: value}, nil
	}
	first, err := cache.get(t.Context(), key, chunkRequest{length: 1}, load)
	requirements.NoError(err)
	continued, err := cache.get(t.Context(), key, chunkRequest{offset: 1, length: 1, digest: first.digest}, load)
	requirements.NoError(err)
	assertions.Equal(first.etag, continued.etag)
	assertions.Equal(1, reads)
	value = "second"
	_, err = cache.get(t.Context(), key, chunkRequest{length: 1, digest: first.digest}, load)
	requirements.ErrorIs(err, errDownloadExpired)
	second, err := cache.get(t.Context(), key, chunkRequest{length: 1}, load)
	requirements.NoError(err)
	assertions.Equal("second", second.etag)
	assertions.Equal(3, reads)
	attachmentKey := downloadKey{attachment: 1}
	attachment, err := cache.get(t.Context(), attachmentKey, chunkRequest{length: 1}, func() (*downloadSnapshot, error) {
		return &downloadSnapshot{data: []byte("attachment")}, nil
	})
	requirements.NoError(err)
	for personID := range maxDownloads {
		_, err = cache.get(t.Context(), downloadKey{operation: "get_person_record", identity: strconv.Itoa(personID)}, chunkRequest{length: 1024}, func() (*downloadSnapshot, error) {
			return &downloadSnapshot{data: []byte("small profile")}, nil
		})
		requirements.NoError(err)
	}
	continued, err = cache.get(t.Context(), attachmentKey, chunkRequest{offset: 1, length: 1, digest: attachment.digest}, load)
	requirements.NoError(err)
	assertions.Equal(attachment.data, continued.data)
}
