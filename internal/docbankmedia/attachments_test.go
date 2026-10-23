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

func TestVaultUID(t *testing.T) {
	for _, body := range []string{`{"vault_id":"00000000-0000-4000-8000-000000000010"}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/info", r.URL.Path)
				assert.Equal(http.MethodGet, r.Method)
				_, _ = io.WriteString(w, body)
			}))
			defer remote.Close()
			client, err := NewClient(remote.URL, nil)
			require.NoError(err)
			vault, err := client.VaultUID(t.Context())
			if body == `{}` {
				require.ErrorIs(err, ErrInvalidReceipt)
				return
			}
			require.NoError(err)
			assert.Equal("00000000-0000-4000-8000-000000000010", vault)
		})
	}
}
