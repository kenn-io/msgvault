package docbankmedia

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/docbanktest"
)

func TestAttachmentUploadDigestContract(t *testing.T) {
	t.Parallel()
	content := "synthetic document"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	for _, invalid := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalid), func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			uploads := docbanktest.NewUploadHandler(t)
			if invalid {
				uploads.UploadFaults.Store(hash, docbanktest.FaultDigestMismatch)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(hash, r.Header.Get("X-Docbank-Blob-Hash"))
				assert.Equal("18", r.Header.Get("X-Docbank-Blob-Size"))
				assert.Equal("key", r.Header.Get("X-Api-Key"))
				assert.Equal(hash+".pdf", r.URL.Query().Get("name"))
				uploads.ServeHTTP(w, r)
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, func() (string, error) { return "key", nil })
			require.NoError(err)
			receipt, err := client.UploadAttachment(context.Background(), 7, hash+".pdf", "application/pdf", hash, 18, strings.NewReader(content))
			if invalid {
				assert.ErrorIs(err, ErrInvalidReceipt)
			} else {
				require.NoError(err)
				assert.Equal(int64(8), receipt.Node.ID)
				uploaded, ok := uploads.Uploaded.Load(hash)
				require.True(ok)
				assert.Equal([2]string{hash + ".pdf", "application/pdf"}, uploaded)
			}
		})
	}
}

func TestAttachmentUploadTooLargeLeavesMediaReasonUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, nil)
	require.NoError(err)
	_, err = client.UploadAttachment(t.Context(), 7, "file.pdf", "application/pdf", strings.Repeat("a", 64), 1, strings.NewReader("x"))
	require.Error(err)
	assert.Equal("content_too_large", ErrorCode(err))
	_, err = client.Submit(t.Context(), SuppliedMetadata{OperationID: "operation", Filename: "audio.wav", MediaType: "audio/wav"}, strings.NewReader("x"))
	require.Error(err)
	assert.Equal("http_error", ErrorCode(err))
}

func TestEnsureAttachmentFolderCreatesParents(t *testing.T) {
	t.Parallel()
	for _, invalid := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalid), func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			var created []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				assert.Equal("/api/v1/path/mkdir", r.URL.Path)
				var body struct {
					Path string `json:"path"`
				}
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				created = append(created, body.Path)
				if invalid && body.Path == "/msgvault/files" {
					_, _ = io.WriteString(w, `{"kind":"dir"}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"id":%d,"kind":"dir"}`, len(created)+1)
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, nil)
			require.NoError(err)
			id, err := client.EnsureFolder(t.Context(), "/msgvault/files")
			if invalid {
				require.ErrorIs(err, ErrInvalidReceipt)
			} else {
				require.NoError(err)
				assert.Equal(int64(3), id)
			}
			assert.Equal([]string{"/msgvault", "/msgvault/files"}, created)
		})
	}
}

func TestVaultUIDCanceledLookupDuringAnotherRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	started, release := make(chan struct{}), make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/info", r.URL.Path)
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"vault_id":"vault"}`))
	}))
	defer remote.Close()
	client, err := NewClient(remote.URL, nil)
	require.NoError(err)
	first := make(chan error, 1)
	secondDone := make(chan struct{})
	go func() {
		_, err := client.VaultUID(t.Context())
		first <- err
	}()
	defer func() {
		close(release)
		assert.NoError(<-first)
		<-secondDone
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		assert.Fail("first vault lookup did not reach Docbank")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() {
		defer close(secondDone)
		_, err := client.VaultUID(ctx)
		second <- err
	}()
	select {
	case err := <-second:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(5 * time.Second):
		assert.Fail("canceled lookup waited for another HTTP request")
	}
}

func TestVaultUIDCacheAndSearchRejection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/info" {
			calls++
			_, _ = io.WriteString(w, `{"vault_id":"vault"}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer remote.Close()
	client, err := NewClient(remote.URL, nil)
	require.NoError(err)
	for range 2 {
		vault, err := client.VaultUID(t.Context())
		require.NoError(err)
		assert.Equal("vault", vault)
	}
	assert.Equal(1, calls)
	_, err = client.SearchDocuments(t.Context(), SearchRequest{Query: "quartz", Mode: "lexical", ContentFirst: true, Limit: 20, Profile: "supplied-transcript", Fence: &SearchFence{VaultUID: "vault", ContentVersionIDs: []string{"version"}}})
	require.Error(err)
	_, err = client.VaultUID(t.Context())
	require.NoError(err)
	assert.Equal(2, calls)
}

func TestVaultUID(t *testing.T) {
	for _, body := range []string{`{"vault_id":"vault"}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(http.MethodGet, r.Method)
				assert.Equal("/api/v1/info", r.URL.Path)
				_, _ = io.WriteString(w, body)
			}))
			defer remote.Close()
			client, err := NewClient(remote.URL, nil)
			require.NoError(err)
			vault, err := client.VaultUID(t.Context())
			if body == `{}` {
				require.ErrorIs(err, ErrInvalidReceipt)
			} else {
				require.NoError(err)
				assert.Equal("vault", vault)
			}
		})
	}
}
