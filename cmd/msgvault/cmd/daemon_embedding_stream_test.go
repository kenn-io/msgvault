//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type embeddingStreamFailure struct {
	*httptest.ResponseRecorder

	flushOnly bool
	armed     bool
	second    <-chan struct{}
	ctx       context.Context
	err       error
}

func (w *embeddingStreamFailure) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "progress:") {
		w.armed = true
	}
	if w.armed && !w.flushOnly {
		return 0, w.fail()
	}
	n, err := w.ResponseRecorder.Write(p)
	if err != nil {
		return n, fmt.Errorf("record stream event: %w", err)
	}
	return n, nil
}
func (w *embeddingStreamFailure) FlushError() error {
	if w.armed && w.flushOnly {
		return w.fail()
	}
	w.Flush()
	return nil
}
func (w *embeddingStreamFailure) fail() error {
	select {
	case <-w.second:
		return w.err
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

// This exercises the real child and endpoint. Only the external embedding
// provider and failed HTTP transport are synthetic; no command is stubbed.
func TestDaemonEmbeddingResumeStopsOnBrokenStreamAndRetainsProgress(t *testing.T) {
	buildRequire := require.New(t)
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	buildRequire.NoError(err)
	binaryName := "msgvault"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.Command("go", "build", "-tags", "fts5 sqlite_vec", "-o", binary, "./cmd/msgvault")
	build.Dir = root
	out, err := build.CombinedOutput()
	buildRequire.NoError(err, "build: %s", out)
	saved := daemonCLIExecutableResolver
	daemonCLIExecutableResolver = func() (string, error) { return binary, nil }
	t.Cleanup(func() { daemonCLIExecutableResolver = saved })
	for _, flushOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("flush=%t", flushOnly), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			second := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]jsontext.Value
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if calls.Add(1) == 2 {
					close(second)
					select {
					case <-r.Context().Done():
						close(canceled)
						return
					case <-release:
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0,0]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
			}))
			t.Cleanup(provider.Close)
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			contents := fmt.Sprintf(`[data]
data_dir = %q
[server]
api_key = "stream-test-key"
[vector]
enabled = true
[vector.embeddings]
endpoint = %q
model = "example-model"
dimension = 4
batch_size = 1
max_retries = 0
timeout = "3m"
`, dir, provider.URL+"/v1")
			require.NoError(os.WriteFile(configPath, []byte(contents), 0600))
			cfg, err := config.Load(configPath, "")
			require.NoError(err)
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			require.NoError(st.InitSchema())
			_, err = st.DB().Exec(`INSERT INTO sources (id,source_type,identifier) VALUES (1,'gmail','stream@example.test');
INSERT INTO conversations (id,source_id,conversation_type) VALUES (1,1,'email_thread');
INSERT INTO messages (id,conversation_id,source_id,source_message_id,message_type,subject) VALUES (1,1,1,'one','email','First synthetic message'),(2,1,1,'two','email','Second synthetic message');
INSERT INTO message_bodies (message_id,body_text) VALUES (1,'First synthetic message body'),(2,'Second synthetic message body');`)
			require.NoError(err)
			backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(dir, "vectors.db"), MainDB: st.DB(), MainPath: cfg.DatabaseDSN(), Dimension: 4})
			require.NoError(err)
			initialGeneration, err := backend.CreateGeneration(t.Context(), cfg.Vector.Embeddings.Model, 4, cfg.Vector.GenerationFingerprint())
			require.NoError(err)
			require.NoError(backend.Close())
			gate := api.NewSerialOperationGate()
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg, options: invocationOptions{cfgFile: configPath}}, OperationGate: gate, Logger: slog.New(slog.DiscardHandler)})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				releaseOnce.Do(func() { close(release) })
				select {
				case <-done:
				case <-time.After(45 * time.Second):
					t.Log("handler cleanup exceeded cancellation budget")
				}
			})
			body, err := json.Marshal(api.CLIRunRequest{Args: []string{"embeddings", "resume"}})
			require.NoError(err)
			request := func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Api-Key", "stream-test-key")
				return r
			}
			transportErr := errors.New("synthetic broken stream")
			response := &embeddingStreamFailure{ResponseRecorder: httptest.NewRecorder(), flushOnly: flushOnly, second: second, ctx: ctx, err: transportErr}
			go func() { defer close(done); srv.Router().ServeHTTP(response, request().WithContext(ctx)) }()
			select {
			case <-second:
			case <-time.After(60 * time.Second):
				require.FailNow("second provider batch never started", response.Body.String())
			}
			select {
			case <-done:
			case <-time.After(40 * time.Second):
				require.FailNow("stream failure did not stop child while outer context remained live")
			}
			require.NoError(ctx.Err())
			select {
			case <-canceled:
			case <-time.After(10 * time.Second):
				require.FailNow("provider request was not canceled")
			}
			acquireCtx, acquireCancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer acquireCancel()
			releaseGate, ok := gate.BeginWorkContext(acquireCtx)
			require.True(ok)
			releaseGate()
			var first int64
			var secondStamp *int64
			require.NoError(st.DB().QueryRow("SELECT embed_gen FROM messages WHERE id=1").Scan(&first))
			require.Positive(first)
			assert.Equal(int64(initialGeneration), first)
			require.NoError(st.DB().QueryRow("SELECT embed_gen FROM messages WHERE id=2").Scan(&secondStamp))
			assert.Nil(secondStamp)
			releaseOnce.Do(func() { close(release) })
			healthy := httptest.NewRecorder()
			srv.Router().ServeHTTP(healthy, request())
			assert.NotContains(healthy.Body.String(), `"type":"error"`, healthy.Body.String())
			var generation int64
			require.NoError(st.DB().QueryRow("SELECT embed_gen FROM messages WHERE id=2").Scan(&generation))
			assert.Equal(first, generation)
		})
	}
}
