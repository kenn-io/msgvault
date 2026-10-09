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
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/uploads", r.URL.Path)
				assert.Equal("7", r.URL.Query().Get("parent_id"))
				assert.Equal(hash, r.Header.Get("X-Docbank-Blob-Hash"))
				assert.Equal("18", r.Header.Get("X-Docbank-Blob-Size"))
				assert.Equal("key", r.Header.Get("X-Api-Key"))
				parts, err := r.MultipartReader()
				if !assert.NoError(err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				part, err := parts.NextPart()
				if !assert.NoError(err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal("file", part.FormName())
				assert.Equal(hash+".pdf", part.FileName())
				body, err := io.ReadAll(part)
				if !assert.NoError(err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(content, string(body))
				_, err = parts.NextPart()
				assert.ErrorIs(err, io.EOF)
				receiptHash := hash
				if invalid {
					receiptHash = strings.Repeat("0", 64)
				}
				_, _ = fmt.Fprintf(w, `{"status":"added","computed_hash":%q,"computed_size":18,"node":{"id":8,"parent_id":7,"kind":"file","name":%q,"blob_hash":%q,"size":18,"current_version_id":"version"}}`, receiptHash, hash+".pdf", receiptHash)
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
			}
		})
	}
}

func TestEnsureAttachmentCollectionCreatesParents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
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
		_, _ = fmt.Fprintf(w, `{"id":%d,"kind":"dir"}`, len(created)+1)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, nil)
	require.NoError(err)
	id, err := client.EnsureCollection(t.Context(), "/msgvault/files")
	require.NoError(err)
	assert.Equal(int64(3), id)
	assert.Equal([]string{"/msgvault", "/msgvault/files"}, created)
	_, err = client.EnsureCollection(t.Context(), "/msgvault/../elsewhere")
	assert.ErrorIs(err, ErrInvalidRequest)
}
