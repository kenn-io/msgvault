package matrix

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	matrixattachment "maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestImportMediaCASShapedURLIsNotLocalStorage(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, state := range []attachmentpolicy.DownloadState{attachmentpolicy.StatePending, attachmentpolicy.StateSkipped, attachmentpolicy.StateFailed} {
			t.Run(fmt.Sprintf("encrypted=%t/%s", encrypted, state), func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				hash := strings.Repeat("d", 64)
				uri := "dd/" + hash
				content := &event.MessageEventContent{MsgType: event.MsgFile, Body: "file", URL: id.ContentURIString(uri)}
				if encrypted {
					content.URL = ""
					content.File = &event.EncryptedFileInfo{EncryptedFile: *matrixattachment.NewEncryptedFile(), URL: id.ContentURIString(uri)}
				}
				raw, err := json.Marshal(&event.Event{ID: "$media", Sender: "@member:example.org", Type: event.EventMessage, Content: event.Content{Parsed: content}})
				req.NoError(err)
				f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
				opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), NoMedia: state == attachmentpolicy.StatePending}
				if state == attachmentpolicy.StateSkipped {
					opts.MediaPolicy.Scope = attachmentpolicy.ScopeNone
				}
				_, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
				req.NoError(err)
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
				req.NoError(err)
				refs, err := f.store.MessageMatrixAttachments(ids["$media"])
				req.NoError(err)
				req.Len(refs, 1)
				ref := refs["matrix:"+uri]
				check.Equal(state, ref.State)
				check.Equal(ref.SourceAttachmentID, ref.StoragePath)
				check.Empty(ref.ContentHash)
				matches, err := query.NewEngine(f.store.DB(), f.store.IsPostgreSQL()).GetAttachmentsByHash(t.Context(), hash)
				req.NoError(err)
				check.Empty(matches)
			})
		}
	}
}

func TestImportMediaPolicyReconcilesBeforeDownloadGates(t *testing.T) {
	for _, state := range []attachmentpolicy.DownloadState{attachmentpolicy.StatePending, attachmentpolicy.StateFailed} {
		for _, noMedia := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-media=%t", state, noMedia), func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
				runtime := backupRuntime(t, f.server.URL, f.backup)
				opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
				_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.NoError(err)
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
				req.NoError(err)
				refs, err := f.store.MessageMatrixAttachments(ids["$media"])
				req.NoError(err)
				ref := refs["matrix:mxc://example.org/file"]
				var metadata matrixMediaMetadata
				req.NoError(json.Unmarshal([]byte(ref.Metadata), &metadata))
				metadata.Attempts, metadata.RetryAfter = 2, time.Now().Add(24*time.Hour)
				encoded, err := json.Marshal(metadata)
				req.NoError(err)
				ref.State, ref.Metadata = state, string(encoded)
				req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), ids["$media"], []store.AttachmentRef{ref}))
				f.quiet, f.left = true, true
				mux, ok := f.server.Config.Handler.(*http.ServeMux)
				req.True(ok)
				downloads := 0
				mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
					downloads++
					_, _ = w.Write([]byte("file"))
				})
				opts.NoMedia, opts.MediaPolicy.Scope = noMedia, attachmentpolicy.ScopeNone
				for range 2 {
					sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
					req.NoError(err)
					refs, err = f.store.MessageMatrixAttachments(ids["$media"])
					req.NoError(err)
					ref = refs[ref.SourceAttachmentID]
					check.Equal(attachmentpolicy.StateSkipped, ref.State)
					check.Equal(attachmentpolicy.SkipPolicyScope, ref.SkipReason)
					check.JSONEq(string(encoded), ref.Metadata, "policy changes preserve the retry schedule")
					check.Zero(sum.AttachmentsPending)
					check.Zero(sum.AttachmentsFailed)
				}
				check.Zero(downloads)
			})
		}
	}
}

func TestMatrixMediaPersistedFingerprintGolden(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			req := require.New(t)
			f := newRecoveryFixture(t)
			content := &event.MessageEventContent{MsgType: event.MsgFile, Body: "file", URL: "mxc://example.org/file"}
			want := "sha256:v1:98ef95f84660b34795109c1370d19e8526b70b41b62bf8bfd051dbe3d3aafc73"
			if encrypted {
				content.URL = ""
				content.File = &event.EncryptedFileInfo{
					URL:        "mxc://example.org/file",
					Key:        matrixattachment.JSONWebKey{Key: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Algorithm: "A256CTR"},
					InitVector: "AAAAAAAAAAAAAAAAAAAAAA", Hashes: matrixattachment.EncryptedFileHashes{SHA256: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}, Version: "v2",
				}
				want = "sha256:v1:670ed127a639784b66a8433505e5b171fc14a8d4cd4cdd5252565c0dc0228593"
			}
			raw, err := json.Marshal(&event.Event{ID: "$media", Sender: "@member:example.org", Type: event.EventMessage, Content: event.Content{Parsed: content}})
			req.NoError(err)
			f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
			_, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), ImportOptions{UserID: "@archive:example.org", NoMedia: true})
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			refs, err := f.store.MessageMatrixAttachments(ids["$media"])
			req.NoError(err)
			var metadata matrixMediaMetadata
			req.NoError(json.Unmarshal([]byte(refs["matrix:mxc://example.org/file"].Metadata), &metadata))
			assert.Equal(t, want, metadata.Input)
		})
	}
}

func TestImportMediaEmptyEncryptedFieldsRejectStoredPlainMedia(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	downloads := 0
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		_, _ = w.Write([]byte("file"))
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir()}
	_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	var before matrixMediaMetadata
	req.NoError(json.Unmarshal([]byte(refs["matrix:mxc://example.org/file"].Metadata), &before))
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.file","body":"file","file":{"url":"mxc://example.org/file","key":{"alg":"A256CTR"}}},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)}
	_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	refs, err = f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	ref := refs["matrix:mxc://example.org/file"]
	var after matrixMediaMetadata
	req.NoError(json.Unmarshal([]byte(ref.Metadata), &after))
	check.Equal(before.Input, after.Input, "empty encryption fields share the plain-media fingerprint")
	check.Equal(attachmentpolicy.StateFailed, ref.State)
	check.Empty(ref.ContentHash)
	check.Equal(1, downloads, "invalid keys are rejected before a new request")
}

func TestImportMediaCaptionEditKeepsPNGType(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, declared := range []bool{false, true} {
			t.Run(fmt.Sprintf("encrypted=%t/declared=%t", encrypted, declared), func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				var pngData bytes.Buffer
				req.NoError(png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 1, 1))))
				data := append([]byte(nil), pngData.Bytes()...)
				content := &event.MessageEventContent{MsgType: event.MsgImage, Body: "image.png", URL: "mxc://example.org/image"}
				if declared {
					content.Info = &event.FileInfo{MimeType: "image/png"}
				}
				if encrypted {
					file := matrixattachment.NewEncryptedFile()
					file.EncryptInPlace(data)
					content.URL = ""
					content.File = &event.EncryptedFileInfo{EncryptedFile: *file, URL: "mxc://example.org/image"}
				}
				raw, err := json.Marshal(&event.Event{ID: "$media", Sender: "@member:example.org", Timestamp: 10, Type: event.EventMessage, Content: event.Content{Parsed: content}})
				req.NoError(err)
				f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
				mux, ok := f.server.Config.Handler.(*http.ServeMux)
				req.True(ok)
				downloads := 0
				mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/image", func(w http.ResponseWriter, _ *http.Request) {
					downloads++
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(data)
				})
				runtime := backupRuntime(t, f.server.URL, f.backup)
				opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir()}
				_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.NoError(err)
				content.Info = nil
				content.Body = "edited caption"
				raw, err = json.Marshal(&event.Event{ID: "$edit", Sender: "@member:example.org", Timestamp: 20, Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{NewContent: content, RelatesTo: &event.RelatesTo{Type: event.RelReplace, EventID: "$media"}}}})
				req.NoError(err)
				f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
				_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.NoError(err)
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
				req.NoError(err)
				refs, err := f.store.MessageMatrixAttachments(ids["$media"])
				req.NoError(err)
				req.Len(refs, 1)
				for _, ref := range refs {
					check.Equal("image/png", ref.MimeType)
					check.Equal(pngData.Len(), ref.Size, "caption edits without declared size keep the stored size")
					stored, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, ref.StoragePath))
					req.NoError(err)
					check.Equal(pngData.Bytes(), stored)
				}
				check.Equal(1, downloads)
			})
		}
	}
}

func TestImportFilteredMediaDiscoveryRunsOnce(t *testing.T) {
	for _, filter := range []string{"include", "exclude"} {
		t.Run(filter, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$text","sender":"@member:example.org","content":{"msgtype":"m.text","body":"ordinary text"}}`)}
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true}
			if filter == "include" {
				opts.Rooms = []string{"!room:example.org"}
			} else {
				opts.ExcludeRooms = []string{"!other:example.org"}
			}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			previous, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			state, err := loadSyncState(previous.CursorAfter.String)
			req.NoError(err)
			check.Equal(mediaDiscoveryVersion, state.MediaDiscoveryVersion)
			check.Equal(mediaDiscoveryScope(opts), state.MediaDiscoveryScope)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$text"})
			req.NoError(err)
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			lookups := 0
			mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/event/$edit", func(w http.ResponseWriter, _ *http.Request) {
				lookups++
				writeMatrixJSON(t, w, matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$text"}}}`))
			})
			legacy := sql.NullString{String: `{"matrix_edit_event_id":"$edit","matrix_edit_ts":20}`, Valid: true}
			req.NoError(f.store.SetMessageMetadata(ids["$text"], legacy))
			f.quiet, f.left = true, true
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Zero(lookups, "the completed scope skips discovery")
			if filter == "include" {
				opts.Rooms = append(opts.Rooms, "!other:example.org")
			} else {
				opts.ExcludeRooms = append(opts.ExcludeRooms, "!another:example.org")
			}
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(1, lookups, "a changed scope repeats discovery")
			req.NoError(f.store.SetMessageMetadata(ids["$text"], legacy))
			if filter == "include" {
				opts.Rooms = []string{"!other:example.org", "!room:example.org", "!room:example.org"}
			} else {
				opts.ExcludeRooms = []string{"!another:example.org", "!other:example.org", "!other:example.org"}
			}
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(1, lookups, "reordered and repeated filters keep the completed scope")
		})
	}
}

func TestImportMediaValidatesMXCBeforeRequest(t *testing.T) {
	for _, uri := range []struct {
		value string
		valid bool
	}{
		{"mxc://example.org/Az09_-", true},
		{"mxc://example.org:8448/file", true},
		{"mxc://127.0.0.1/file", true},
		{"mxc://[2001:db8::1]:8448/file", true},
		{"mxc://./file", false},
		{"mxc://../file", false},
		{"mxc://example.org/../file", false},
		{"mxc://example.org/%2e%2e", false},
		{"mxc://example.org/file?query", false},
		{"mxc://example.org/file#fragment", false},
		{"mxc://example.org/fïle", false},
		{"mxc://user@example.org/file", false},
		{"mxc://bad_host/file", false},
		{"mxc://example.org:port/file", false},
		{"mxc://example.org:123456/file", false},
		{"mxc://[:::]/file", false},
		{"mxc:///file", false},
	} {
		t.Run("request/"+uri.value, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":%q}}`, uri.value))}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			requests := 0
			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write([]byte("file"))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			if uri.valid {
				check.Equal(1, requests)
				check.Equal(int64(1), sum.AttachmentsDownloaded)
				check.Zero(sum.AttachmentsFailed)
				return
			}
			check.Zero(requests)
			check.Equal(int64(1), sum.AttachmentsFailed)
			check.Zero(sum.AttachmentsDownloaded)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			refs, err := f.store.MessageMatrixAttachments(ids["$media"])
			req.NoError(err)
			check.Equal(attachmentpolicy.StateFailed, refs["matrix:"+uri.value].State)
			opts.Full = true
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Zero(requests)
		})
	}
}

func TestImportMediaStopsAfterConsecutiveTransientFailures(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("success-reset=%t", reset), func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			for i := range 7 {
				f.timeline = append(f.timeline, matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.message","event_id":"$media%d","sender":"@member:example.org","origin_server_ts":%d,"content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file%d"}}`, i, i+1, i)))
			}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			requests := 0
			unavailable := true
			mux.HandleFunc("GET /_matrix/client/v1/media/download/", func(w http.ResponseWriter, _ *http.Request) {
				requests++
				if unavailable && (!reset || requests != 3) {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte("file"))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			if reset {
				check.Equal(6, requests)
				check.Equal(int64(1), sum.AttachmentsDownloaded)
				check.Equal(int64(6), sum.AttachmentsPending)
			} else {
				check.Equal(3, requests)
				check.Equal(int64(7), sum.AttachmentsPending)
			}
			check.Zero(sum.AttachmentsFailed)
			unavailable = false
			f.quiet, f.left = true, true
			sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			if reset {
				check.Equal(int64(1), sum.AttachmentsDownloaded)
				check.Equal(int64(5), sum.AttachmentsPending)
			} else {
				check.Equal(int64(4), sum.AttachmentsDownloaded)
				check.Equal(int64(3), sum.AttachmentsPending)
			}
		})
	}
}

func TestImportMediaNegativeDeclaredSizeContinuesSync(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.file","body":"file.txt","url":"mxc://example.org/file","info":{"size":-1}}}`),
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$text","sender":"@member:example.org","origin_server_ts":20,"content":{"msgtype":"m.text","body":"after malformed size"}}`),
	}
	runtime := backupRuntime(t, f.server.URL, f.backup)
	_, err := NewImporter(f.store, runtime).Import(t.Context(), ImportOptions{UserID: "@archive:example.org", MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}})
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media", "$text"})
	req.NoError(err)
	req.Len(ids, 2)
	refs, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	req.Contains(refs, "matrix:mxc://example.org/file")
	check.Zero(refs["matrix:mxc://example.org/file"].Size)
	check.Equal("after malformed size", f.body(t, "$text"))
}

func TestImportMediaEncryptionReplacementAndReuse(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	plaintext := []byte("synthetic decrypted file")
	ciphertext := append([]byte(nil), plaintext...)
	file := matrixattachment.NewEncryptedFile()
	file.EncryptInPlace(ciphertext)
	content := &event.MessageEventContent{MsgType: event.MsgFile, Body: "file", File: &event.EncryptedFileInfo{EncryptedFile: *file, URL: "mxc://example.org/file"}}
	raw, err := json.Marshal(map[string]any{
		"type": "m.room.message", "event_id": "$edit", "sender": "@member:example.org", "origin_server_ts": 20,
		"content": map[string]any{"msgtype": "m.file", "m.new_content": content, "m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$media"}},
	})
	req.NoError(err)
	f.timeline = []*event.Event{
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.file","body":"ciphertext","url":"mxc://example.org/file"}}`),
		matrixTestEvent(t, string(raw)),
	}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	requests := 0
	missing := false
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if missing {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(ciphertext)
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(2, requests)
	check.Equal(int64(2), sum.AttachmentsDownloaded)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	check.NotContains(refs["matrix:mxc://example.org/file"].Metadata, file.Key.Key)
	check.NotContains(refs["matrix:mxc://example.org/file"].Metadata, file.InitVector)
	var metadata matrixMediaMetadata
	req.NoError(json.Unmarshal([]byte(refs["matrix:mxc://example.org/file"].Metadata), &metadata))
	stored, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs["matrix:mxc://example.org/file"].StoragePath))
	req.NoError(err)
	check.Equal(plaintext, stored)
	opts.Full = true
	_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(2, requests, "unchanged encryption instructions reuse the stored bytes")
	missing = true
	content.File.Key.KeyOps = []string{"decrypt", "encrypt"}
	raw, err = json.Marshal(map[string]any{
		"type": "m.room.message", "event_id": "$reordered", "sender": "@member:example.org", "origin_server_ts": 30,
		"content": map[string]any{"msgtype": "m.file", "m.new_content": content, "m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$media"}},
	})
	req.NoError(err)
	f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
	opts.Full = false
	_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(2, requests, "reordered key operations reuse bytes after the remote file disappears")
	refs, err = f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	check.Equal(attachmentpolicy.StateStored, refs["matrix:mxc://example.org/file"].State)
	stored, err = os.ReadFile(filepath.Join(opts.AttachmentsDir, refs["matrix:mxc://example.org/file"].StoragePath))
	req.NoError(err)
	check.Equal(plaintext, stored)
	t.Run("newer fingerprint", func(t *testing.T) {
		req := require.New(t)
		ref := refs["matrix:mxc://example.org/file"]
		metadata.Input = strings.Replace(metadata.Input, "sha256:v1:", "sha256:v2:", 1)
		encoded, err := json.Marshal(metadata)
		req.NoError(err)
		ref.Metadata = string(encoded)
		req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), ids["$media"], []store.AttachmentRef{ref}))
		refs, err = f.store.MessageMatrixAttachments(ids["$media"])
		req.NoError(err)
		ref = refs["matrix:mxc://example.org/file"]
		bodyBefore := f.body(t, "$media")
		metadataBefore, err := f.store.GetMessageMetadata(ids["$media"])
		req.NoError(err)
		var editedBefore bool
		req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT is_edited FROM messages WHERE id = ?`), ids["$media"]).Scan(&editedBefore))
		for i, replacement := range []*event.MessageEventContent{
			{MsgType: event.MsgFile, Body: "newer body", File: content.File},
			{MsgType: event.MsgFile, Body: "different URL", URL: "mxc://example.org/other"},
			{MsgType: event.MsgText, Body: "text replacement"},
			{MsgType: event.MsgFile, Body: "missing URL"},
		} {
			t.Run(replacement.Body, func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				content := replacement
				raw, err = json.Marshal(map[string]any{
					"type": "m.room.message", "event_id": fmt.Sprintf("$newer%d", i), "sender": "@member:example.org", "origin_server_ts": 40,
					"content": map[string]any{"msgtype": "m.file", "m.new_content": content, "m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$media"}},
				})
				req.NoError(err)
				f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
				_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.ErrorContains(err, "unsupported Matrix media fingerprint format")
				check.Equal(bodyBefore, f.body(t, "$media"))
				metadataAfter, err := f.store.GetMessageMetadata(ids["$media"])
				req.NoError(err)
				check.Equal(metadataBefore, metadataAfter)
				var editedAfter bool
				req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT is_edited FROM messages WHERE id = ?`), ids["$media"]).Scan(&editedAfter))
				check.Equal(editedBefore, editedAfter)
				check.Equal(2, requests, "a newer fingerprint leaves archived media alone when the remote file returns 404")
				refs, err = f.store.MessageMatrixAttachments(ids["$media"])
				req.NoError(err)
				check.Equal(ref, refs["matrix:mxc://example.org/file"])
				stored, err = os.ReadFile(filepath.Join(opts.AttachmentsDir, refs["matrix:mxc://example.org/file"].StoragePath))
				req.NoError(err)
				check.Equal(plaintext, stored)
			})
		}
	})
}

func TestImportMediaAlgorithmChangeInvalidatesFingerprint(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	data := []byte("synthetic file")
	file := matrixattachment.NewEncryptedFile()
	file.EncryptInPlace(data)
	content := &event.MessageEventContent{MsgType: event.MsgFile, Body: "file", File: &event.EncryptedFileInfo{EncryptedFile: *file, URL: "mxc://example.org/file"}}
	raw, err := json.Marshal(map[string]any{"type": "m.room.message", "event_id": "$media", "sender": "@member:example.org", "origin_server_ts": 10, "content": content})
	req.NoError(err)
	f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	requests := 0
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(data)
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	before, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	content.File.Key.Algorithm = "unsupported"
	raw, err = json.Marshal(map[string]any{
		"type": "m.room.message", "event_id": "$edit", "sender": "@member:example.org", "origin_server_ts": 20,
		"content": map[string]any{"m.new_content": content, "m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$media"}},
	})
	req.NoError(err)
	f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
	sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	ref := refs["matrix:mxc://example.org/file"]
	check.Equal(attachmentpolicy.StateFailed, ref.State)
	check.Equal(attachmentpolicy.SkipFetchFailure, ref.SkipReason)
	check.Empty(ref.ContentHash)
	var beforeMetadata, afterMetadata matrixMediaMetadata
	req.NoError(json.Unmarshal([]byte(before[ref.SourceAttachmentID].Metadata), &beforeMetadata))
	req.NoError(json.Unmarshal([]byte(ref.Metadata), &afterMetadata))
	check.NotEqual(beforeMetadata.Input, afterMetadata.Input)
	check.Equal(beforeMetadata.Attempts, afterMetadata.Attempts)
	check.Equal(int64(1), sum.AttachmentsFailed)
	check.Equal(1, requests)
	content.File.Key.Algorithm = "A256CTR"
	raw, err = json.Marshal(map[string]any{
		"type": "m.room.message", "event_id": "$corrected", "sender": "@member:example.org", "origin_server_ts": 30,
		"content": map[string]any{"m.new_content": content, "m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$media"}},
	})
	req.NoError(err)
	f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
	sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(int64(1), sum.AttachmentsDownloaded)
	check.Zero(sum.AttachmentsFailed)
	check.Equal(2, requests)
	refs, err = f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	ref = refs["matrix:mxc://example.org/file"]
	check.Equal(attachmentpolicy.StateStored, ref.State)
	req.NoError(json.Unmarshal([]byte(ref.Metadata), &afterMetadata))
	check.Equal(beforeMetadata.Input, afterMetadata.Input, "the default algorithm keeps the existing fingerprint")
}

func TestImportDeferredMediaSurvivesSourceRedaction(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	requests := 0
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("file"))
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(int64(1), sum.AttachmentsPending)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","redacts":"$media","content":{}}`)}
	sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(int64(1), sum.AttachmentsPending)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	var deleted bool
	req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), ids["$media"]).Scan(&deleted))
	check.True(deleted)
	f.quiet = true
	opts.NoMedia = false
	sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(1, requests)
	check.Zero(sum.AttachmentsPending)
	check.Equal(int64(1), sum.AttachmentsDownloaded)
}

func TestImportMediaContentRefusesNewerVersion(t *testing.T) {
	for _, path := range []string{"edit", "retry", "legacy"} {
		t.Run(path, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"original","url":"mxc://example.org/file"}}`)}
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			messageID := ids["$media"]
			metadata := sql.NullString{String: `{"matrix_media_content_version":2,"matrix_media_content":{"future_format":true}}`, Valid: true}
			req.NoError(f.store.SetMessageMetadata(messageID, metadata))
			metadata, err = f.store.GetMessageMetadata(messageID)
			req.NoError(err)
			switch path {
			case "legacy":
				opts.Full = true
				req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), messageID, nil))
			case "retry":
				f.quiet = true
			default:
				f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.file","body":"replacement","url":"mxc://example.org/file"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)}
			}
			before, err := f.store.MessageMatrixAttachments(messageID)
			req.NoError(err)
			body := f.body(t, "$media")
			opts.NoMedia = false
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.ErrorContains(err, "unsupported Matrix media content version 2")
			check.Equal(body, f.body(t, "$media"))
			after, err := f.store.MessageMatrixAttachments(messageID)
			req.NoError(err)
			check.Equal(before, after)
			saved, err := f.store.GetMessageMetadata(messageID)
			req.NoError(err)
			check.Equal(metadata, saved)
			var edited bool
			req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT is_edited FROM messages WHERE id = ?`), messageID).Scan(&edited))
			check.False(edited)
		})
	}
}

func TestImportMediaRetrySkipsMalformedRow(t *testing.T) {
	for _, malformed := range []string{"event", "metadata"} {
		t.Run(malformed, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			for _, name := range []string{"broken", "valid"} {
				f.timeline = append(f.timeline, matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.message","event_id":"$%s","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/%s"}}`, name, name)))
			}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			requests := 0
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/valid", func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write([]byte("file"))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$broken"})
			req.NoError(err)
			if malformed == "event" {
				req.NoError(f.store.UpsertMessageRawWithFormat(ids["$broken"], []byte("malformed"), rawFormat))
			} else {
				req.NoError(f.store.SetMessageMetadata(ids["$broken"], sql.NullString{String: `{"matrix_media_content":42}`, Valid: true}))
			}
			f.quiet = true
			opts.NoMedia = false
			sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(1, requests)
			check.Equal(int64(1), sum.AttachmentsDownloaded)
			check.Equal(int64(1), sum.AttachmentsPending)
			source, err := f.store.GetSourceByTypeAndIdentifier(SourceType, opts.UserID)
			req.NoError(err)
			state, err := loadSyncState(source.SyncCursor.String)
			req.NoError(err)
			check.Equal("next", state.NextBatch)
		})
	}
}

type mediaErrorTransport struct {
	http.RoundTripper

	err      error
	body     bool
	requests int
	deadline time.Duration
	length   int64
	reads    int
	content  string
}

func (m *mediaErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.Contains(req.URL.Path, "/media/download/") {
		return m.RoundTripper.RoundTrip(req)
	}
	m.requests++
	deadline, _ := req.Context().Deadline()
	m.deadline = time.Until(deadline)
	if !m.body {
		return nil, m.err
	}
	if m.content != "" {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: int64(len(m.content)), Body: io.NopCloser(strings.NewReader(m.content)), Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: m.length, Body: io.NopCloser(mediaErrorReader{err: m.err, reads: &m.reads}), Request: req}, nil
}

type mediaErrorReader struct {
	err   error
	reads *int
}

func (m mediaErrorReader) Read([]byte) (int, error) {
	*m.reads++
	return 0, m.err
}

func TestImportMediaRejectsOversizeContentLengthBeforeReading(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	runtime := backupRuntime(t, f.server.URL, f.backup)
	transport := &mediaErrorTransport{RoundTripper: http.DefaultTransport, err: io.ErrUnexpectedEOF, body: true, length: 100}
	runtime.Client.Client.Transport = transport
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 4}}
	sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(1, transport.requests)
	check.Zero(transport.reads)
	check.Zero(sum.AttachmentsPending)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	ref := refs["matrix:mxc://example.org/file"]
	check.Equal(attachmentpolicy.StateSkipped, ref.State)
	check.Equal(attachmentpolicy.SkipSizeCap, ref.SkipReason)
	check.Equal(100, ref.Size)
}

func TestImportMediaBodyUsesDownloadTimeout(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	f.quiet = true
	transport := &mediaErrorTransport{RoundTripper: http.DefaultTransport, body: true, content: "body"}
	runtime.Client.Client.Transport = transport
	runtime.Client.Client.Timeout = time.Second
	opts.NoMedia = false
	sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(int64(1), sum.AttachmentsDownloaded)
	check.Zero(sum.AttachmentsPending)
	check.Greater(transport.deadline, runtime.Client.Client.Timeout)
	check.InDelta(attachmentpolicy.DownloadTimeout(attachmentpolicy.DefaultChatMaxBytes), transport.deadline, float64(time.Second))
	check.Equal(time.Second, runtime.Client.Client.Timeout)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs["matrix:mxc://example.org/file"].StoragePath))
	req.NoError(err)
	check.Equal("body", string(data))
}

func TestImportMediaTransferErrorContinuesAndRetriesOnNormalSync(t *testing.T) {
	for _, body := range []bool{false, true} {
		for _, transferErr := range []error{context.DeadlineExceeded, context.Canceled} {
			t.Run(fmt.Sprintf("body=%t/%s", body, transferErr), func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				f.timeline = []*event.Event{
					matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.image","body":"photo.jpg","url":"mxc://example.org/photo"}}`),
					matrixTestEvent(t, `{"type":"m.room.message","event_id":"$text","sender":"@member:example.org","content":{"msgtype":"m.text","body":"later text"}}`),
				}
				runtime := backupRuntime(t, f.server.URL, f.backup)
				transport := &mediaErrorTransport{RoundTripper: runtime.Client.Client.Transport, err: transferErr, body: body}
				if transport.RoundTripper == nil {
					transport.RoundTripper = http.DefaultTransport
				}
				runtime.Client.Client.Transport = transport
				runtime.Client.DefaultHTTPRetries = 0
				opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 1024}}
				runtime.Client.Client.Timeout = 2 * attachmentpolicy.DownloadTimeout(opts.MediaPolicy.MaxBytes)
				retryStarted := time.Now()
				sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.NoError(err)
				retryEnded := time.Now()
				req.NoError(t.Context().Err())
				check.Equal(int64(1), sum.AttachmentsPending)
				check.Equal(1, transport.requests, "the sweep leaves this run's attempted media alone")
				check.Equal("later text", f.body(t, "$text"))
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
				req.NoError(err)
				refs, err := f.store.MessageMatrixAttachments(ids["$media"])
				req.NoError(err)
				check.Equal(attachmentpolicy.StatePending, refs["matrix:mxc://example.org/photo"].State)
				if body || !errors.Is(transferErr, context.DeadlineExceeded) {
					return
				}
				f.quiet = true
				for attempt, hours := range []int{1, 2, 4, 8, 16, 24, 24} {
					refs, err = f.store.MessageMatrixAttachments(ids["$media"])
					req.NoError(err)
					var metadata matrixMediaMetadata
					req.NoError(json.Unmarshal([]byte(refs["matrix:mxc://example.org/photo"].Metadata), &metadata))
					check.Equal(attempt+1, metadata.Attempts)
					backoff := time.Duration(hours) * time.Hour
					check.False(metadata.RetryAfter.Before(retryStarted.Add(backoff)))
					check.False(metadata.RetryAfter.After(retryEnded.Add(backoff)))
					sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
					req.NoError(err)
					check.Equal(attempt+1, transport.requests, "normal sync waits for the backoff")
					check.Equal(int64(1), sum.AttachmentsPending)
					check.Zero(sum.AttachmentsFailed)
					makeMediaRetryDue(t, f.store, ids["$media"])
					retryStarted = time.Now()
					sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
					req.NoError(err)
					retryEnded = time.Now()
					check.Equal(attempt+2, transport.requests, "normal sync retries after the backoff")
					check.Equal(int64(1), sum.AttachmentsPending)
					check.Zero(sum.AttachmentsFailed)
				}
			})
		}
	}
}

func TestImportMediaVersionSelectionSurvivesCancellation(t *testing.T) {
	for _, selection := range []string{"new", "edit", "redaction"} {
		t.Run(selection, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.quiet, f.left = true, true
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupt := ""
			type observation struct {
				body, source, state, metadata string
				edited                        bool
				err                           error
			}
			observed := make(chan observation, 1)
			for _, name := range []string{"original", "replacement"} {
				mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/"+name, func(w http.ResponseWriter, r *http.Request) {
					if name == interrupt {
						probe, done := context.WithTimeout(t.Context(), 5*time.Second)
						defer done()
						var result observation
						result.err = f.store.DB().QueryRowContext(probe, f.store.Rebind(`SELECT m.snippet, a.source_attachment_id, a.attachment_state, m.is_edited, COALESCE(CAST(m.metadata AS TEXT), '{}') FROM messages m JOIN attachments a ON a.message_id = m.id WHERE m.source_id = ? AND m.source_message_id = '$media'`), f.sourceID).Scan(&result.body, &result.source, &result.state, &result.edited, &result.metadata)
						observed <- result
						w.WriteHeader(http.StatusOK)
						_ = http.NewResponseController(w).Flush()
						cancel()
						<-r.Context().Done()
						return
					}
					_, _ = w.Write([]byte(name))
				})
			}
			imp := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup))
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
			convID, err := f.store.EnsureConversationWithType(f.sourceID, "!room:example.org", "group_chat", "Media")
			req.NoError(err)
			original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"original","url":"mxc://example.org/original"}}`)
			edit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.file","body":"replacement","url":"mxc://example.org/replacement"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)
			selected := original
			interrupt = "original"
			if selection != "new" {
				interrupt = ""
				req.NoError(imp.persistEvent(t.Context(), f.sourceID, convID, original, conversation, opts, &ImportSummary{}))
				selected = edit
				interrupt = "replacement"
			}
			if selection == "redaction" {
				interrupt = ""
				req.NoError(imp.persistEvent(t.Context(), f.sourceID, convID, edit, conversation, opts, &ImportSummary{}))
				imp = NewImporter(f.store, imp.runtime)
				interrupt = "original"
				selected = matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","room_id":"!room:example.org","redacts":"$edit","content":{}}`)
			}
			req.ErrorIs(imp.persistEvent(ctx, f.sourceID, convID, selected, conversation, opts, &ImportSummary{}), context.Canceled)
			result := <-observed
			req.NoError(result.err)
			check.Equal("[file: "+interrupt+"]", result.body)
			check.Equal("matrix:mxc://example.org/"+interrupt, result.source)
			check.Equal(string(attachmentpolicy.StatePending), result.state)
			check.Equal(selection == "edit", result.edited)
			var metadata map[string]any
			req.NoError(json.Unmarshal([]byte(result.metadata), &metadata))
			if selection != "edit" {
				check.NotContains(metadata, editEventKey)
			} else {
				check.Equal("$edit", metadata[editEventKey])
			}
			interrupt = ""
			sum, err := NewImporter(f.store, imp.runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(int64(1), sum.AttachmentsDownloaded)
		})
	}
}

func TestImportMediaHTTPFailuresRetryByState(t *testing.T) {
	for _, failure := range []struct {
		status int
		full   bool
	}{{408, false}, {425, false}, {429, false}, {500, false}, {404, false}, {404, true}} {
		t.Run(strconv.Itoa(failure.status)+"/full="+strconv.FormatBool(failure.full), func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			requests := 0
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
				requests++
				if requests == 1 {
					w.WriteHeader(failure.status)
					return
				}
				_, _ = w.Write([]byte("file"))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			runtime.Client.IgnoreRateLimit = true
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(1, requests)
			permanent := failure.status == 404
			if permanent {
				check.Equal(int64(1), sum.AttachmentsFailed)
				check.Zero(sum.AttachmentsPending)
			} else {
				check.Equal(int64(1), sum.AttachmentsPending)
				check.Zero(sum.AttachmentsFailed)
			}
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			messageID := ids["$media"]
			req.NotZero(messageID)
			f.quiet, f.left = true, true
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(1, requests)
			refsBeforeRetry, err := f.store.MessageMatrixAttachments(messageID)
			req.NoError(err)
			check.Equal(attachmentpolicy.SkipFetchFailure, refsBeforeRetry["matrix:mxc://example.org/file"].SkipReason)
			var retryMetadata matrixMediaMetadata
			req.NoError(json.Unmarshal([]byte(refsBeforeRetry["matrix:mxc://example.org/file"].Metadata), &retryMetadata))
			if permanent {
				check.WithinDuration(time.Now().Add(7*24*time.Hour), retryMetadata.RetryAfter, time.Minute)
			}
			if !failure.full {
				makeMediaRetryDue(t, f.store, messageID)
			}
			opts.Full = failure.full
			sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(2, requests)
			check.Equal(int64(1), sum.AttachmentsDownloaded)
			check.Zero(sum.AttachmentsPending)
			check.Zero(sum.AttachmentsFailed)
			refs, err := f.store.MessageMatrixAttachments(ids["$media"])
			req.NoError(err)
			check.Equal(attachmentpolicy.StateStored, refs["matrix:mxc://example.org/file"].State)
			check.Empty(refs["matrix:mxc://example.org/file"].SkipReason)
		})
	}
}

func TestImportMediaBackoffAvoidsReadsAndUnchangedWrites(t *testing.T) {
	for _, state := range []attachmentpolicy.DownloadState{attachmentpolicy.StatePending, attachmentpolicy.StateFailed, attachmentpolicy.StateStored, attachmentpolicy.StateSkipped} {
		t.Run(string(state), func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			evt := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)
			f.timeline = []*event.Event{evt}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
				switch state {
				case attachmentpolicy.StatePending:
					w.WriteHeader(http.StatusServiceUnavailable)
				case attachmentpolicy.StateFailed:
					w.WriteHeader(http.StatusNotFound)
				default:
					_, _ = w.Write([]byte("file"))
				}
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			if state == attachmentpolicy.StateSkipped {
				opts.MediaPolicy.Scope = attachmentpolicy.ScopeNone
			}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			refs, err := f.store.MessageMatrixAttachments(ids["$media"])
			req.NoError(err)
			req.Equal(state, refs["matrix:mxc://example.org/file"].State)
			if f.store.IsPostgreSQL() {
				_, err = f.store.DB().Exec(`CREATE FUNCTION reject_media_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected media write'; END $$`)
				req.NoError(err)
			}
			for _, table := range []string{"attachments", "messages"} {
				for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
					create := fmt.Sprintf(`CREATE TRIGGER reject_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'unexpected media write'); END`, table, operation, operation, table)
					if f.store.IsPostgreSQL() {
						create = fmt.Sprintf(`CREATE TRIGGER reject_%s_%s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION reject_media_write()`, table, operation, operation, table)
					}
					_, err = f.store.DB().Exec(create)
					req.NoError(err)
				}
			}
			store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
			t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			imp := NewImporter(f.store, runtime)
			if state == attachmentpolicy.StatePending || state == attachmentpolicy.StateFailed {
				req.NoError(imp.retryArchivedMedia(t.Context(), f.sourceID, opts, &ImportSummary{}))
				check.NotContains(logs.String(), "message_raw", "backoff skips raw event reads")
			}
			opts.NoMedia = true
			req.True(parseContent(evt))
			req.NoError(imp.applyMedia(t.Context(), ids["$media"], mediaContent(evt), evt.Type, attachmentpolicy.Conversation{Type: "group_chat"}, opts, &ImportSummary{}))
			req.ErrorContains(f.store.ReplaceMessageMatrixAttachments(t.Context(), ids["$media"], []store.AttachmentRef{refs["matrix:mxc://example.org/file"]}), "unexpected media write")
		})
	}
}

func TestImportLegacyEditAuthenticationFailureKeepsCheckpoint(t *testing.T) {
	for _, response := range []string{"", `{"errcode":"M_UNKNOWN_TOKEN","error":"expired token"}`} {
		t.Run(response, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$legacy","sender":"@member:example.org","content":{"msgtype":"m.text","body":"original"}}`)}
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$legacy"})
			req.NoError(err)
			req.NoError(f.store.SetMessageMetadata(ids["$legacy"], sql.NullString{String: `{"matrix_edit_event_id":"$edit","matrix_edit_ts":20}`, Valid: true}))
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			lookups := 0
			mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/event/$edit", func(w http.ResponseWriter, _ *http.Request) {
				lookups++
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(response))
			})
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("archived bytes"))
			})
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
			f.extraRooms = map[string][]*event.Event{"!earlier:example.org": {matrixTestEvent(t, `{"type":"m.room.message","event_id":"$earlier","sender":"@member:example.org","content":{"msgtype":"m.text","body":"committed earlier"}}`)}}
			opts.Full = true
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.ErrorContains(err, "authenticate legacy Matrix edit lookup")
			check.Equal(1, lookups)
			check.Equal("committed earlier", f.body(t, "$earlier"))
			ids, err = f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			refs, err := f.store.MessageMatrixAttachments(ids["$media"])
			req.NoError(err)
			ref := refs["matrix:mxc://example.org/file"]
			check.Equal(attachmentpolicy.StateStored, ref.State)
			data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, ref.StoragePath))
			req.NoError(err)
			check.Equal("archived bytes", string(data))
			checkpoint, err := f.store.GetLatestCheckpointedSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			state, err := loadSyncState(checkpoint.CursorBefore.String)
			req.NoError(err)
			check.Equal("next", state.NextBatch)
			check.Contains(state.Rooms, "!earlier:example.org")
			check.Contains(state.Rooms, "!room:example.org")
		})
	}
}

func TestImportMediaAuthenticationFailureStopsSync(t *testing.T) {
	for _, response := range []string{"", `{"errcode":"M_UNKNOWN_TOKEN","error":"expired token"}`, `{"errcode":"M_UNRECOGNIZED","error":"unsupported endpoint"}`} {
		t.Run(response, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			var warnings bytes.Buffer
			logger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&warnings, nil)))
			t.Cleanup(func() { slog.SetDefault(logger) })
			f := newRecoveryFixture(t)
			for _, name := range []string{"first", "second"} {
				f.timeline = append(f.timeline, matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.message","event_id":"$%s","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/%s"}}`, name, name)))
			}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			requests := 0
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/{file}", func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				status := http.StatusUnauthorized
				if strings.Contains(response, "M_UNRECOGNIZED") {
					status = http.StatusNotFound
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(response))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), NoMedia: true, MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			f.quiet = true
			opts.NoMedia = false
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			if strings.Contains(response, "M_UNRECOGNIZED") {
				req.NoError(err)
				check.Equal(1, strings.Count(warnings.String(), "lacks authenticated media downloads"))
				f.quiet = false
				f.timeline = append(f.timeline, matrixTestEvent(t, `{"type":"m.room.message","event_id":"$text","sender":"@member:example.org","content":{"msgtype":"m.text","body":"text after media"}}`))
				sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.NoError(err)
				check.Equal("text after media", f.body(t, "$text"))
				check.Equal(int64(2), sum.AttachmentsPending)
				check.Equal(2, requests, "each run probes the endpoint once")
				check.Equal(2, strings.Count(warnings.String(), "lacks authenticated media downloads"))
			} else {
				req.ErrorContains(err, "authenticate Matrix media download")
				check.Equal(1, requests)
			}
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$first", "$second"})
			req.NoError(err)
			req.Len(ids, 2)
			for _, messageID := range ids {
				refs, err := f.store.MessageMatrixAttachments(messageID)
				req.NoError(err)
				for _, ref := range refs {
					check.Equal(attachmentpolicy.StatePending, ref.State)
				}
			}
		})
	}
}

func TestImportMediaMutationsHoldLease(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	var leased atomic.Bool
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
		check.False(leased.Load(), "downloads must release the mutation lease")
		_, _ = w.Write([]byte("archived file"))
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	opts.MediaMutation = func(_ context.Context, write func() error) error {
		check.False(leased.Swap(true), "transactions must acquire the lease before opening rather than nest leases")
		defer leased.Store(false)
		return write()
	}
	_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	messageID := ids["$media"]
	refs, err := f.store.MessageMatrixAttachments(messageID)
	req.NoError(err)
	stored := refs["matrix:mxc://example.org/file"]
	req.NotEmpty(stored.ContentHash)
	for _, replacement := range []string{
		`{"msgtype":"m.file","body":"missing URL"}`,
		`{"msgtype":"m.text","body":"text replacement"}`,
	} {
		t.Run(replacement, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{stored}))
			mutations := 0
			opts.MediaMutation = func(_ context.Context, write func() error) error {
				leased.Store(true)
				defer leased.Store(false)
				if err := write(); err != nil {
					return err
				}
				refs, err := f.store.MessageMatrixAttachments(messageID)
				if err != nil {
					return err
				}
				check.Empty(refs, "the row must clear before releasing the lease")
				mutations++
				return nil
			}
			var content event.MessageEventContent
			req.NoError(json.Unmarshal([]byte(replacement), &content))
			req.NoError(NewImporter(f.store, runtime).applyMedia(t.Context(), messageID, &content, event.EventMessage, attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}, opts, &ImportSummary{}))
			check.Equal(1, mutations)
		})
	}
}

func TestImportMediaReconcilesOncePerTransaction(t *testing.T) {
	for _, state := range []attachmentpolicy.DownloadState{"text", attachmentpolicy.StatePending, attachmentpolicy.StateSkipped, attachmentpolicy.StateStored} {
		t.Run(string(state), func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
			if state == "text" {
				f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$text","sender":"@member:example.org","content":{"msgtype":"m.text","body":"plain"}}`)}
			}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("file bytes"))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			opts.NoMedia = state == attachmentpolicy.StatePending
			if state == attachmentpolicy.StateSkipped {
				opts.MediaPolicy.Scope = attachmentpolicy.ScopeNone
			}
			mutations := 0
			opts.MediaMutation = func(_ context.Context, write func() error) error {
				mutations++
				return write()
			}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			if state == "text" {
				check.Zero(mutations, "text sync leaves the media mutation lease unused")
				return
			}
			if state != attachmentpolicy.StateStored {
				check.Equal(1, mutations, "new deferred media needs only the event transaction")
			}
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.file","body":"renamed","url":"mxc://example.org/file"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)}
			mutations = 0
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(1, mutations, "the edit transaction already reconciled the occurrence")
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			refs, err := f.store.MessageMatrixAttachments(ids["$media"])
			req.NoError(err)
			check.Equal(state, refs["matrix:mxc://example.org/file"].State)
		})
	}
}

func TestImporterTextOnlyEditSkipsLegacyLookups(t *testing.T) {
	for _, kind := range []string{"m.text", "m.file"} {
		t.Run(kind, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{
				matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.message","event_id":"$text","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":%q,"body":"original","url":"mxc://example.org/file"}}`, kind)),
				matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"msgtype":"m.text","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$text"}}}`),
			}
			sum, err := f.run(t, false, false)
			req.NoError(err)
			check.Equal("edited", f.body(t, "$text"))
			check.Zero(sum.AttachmentsPending)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$text"})
			req.NoError(err)
			metadata, err := f.store.GetMessageMetadata(ids["$text"])
			req.NoError(err)
			var values map[string]any
			req.NoError(json.Unmarshal([]byte(metadata.String), &values))
			check.Contains(values, "matrix_media_content")
			check.Equal(map[string]any{"msgtype": "m.text"}, values["matrix_media_content"])
			refs, err := f.store.MessageMatrixAttachments(ids["$text"])
			req.NoError(err)
			check.Empty(refs)
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			lookups := 0
			mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/event/$edit", func(w http.ResponseWriter, _ *http.Request) {
				lookups++
				writeMatrixJSON(t, w, f.timeline[1])
			})
			f.quiet, f.left = true, true
			opts := ImportOptions{UserID: "@archive:example.org", Full: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			if kind == "m.file" {
				req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), ids["$text"], []store.AttachmentRef{{SourceAttachmentID: "matrix:mxc://example.org/file", StoragePath: "mxc://example.org/file", State: attachmentpolicy.StatePending}}))
			}
			for _, noMedia := range []bool{false, false, true} {
				opts.NoMedia = noMedia
				_, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
				req.NoError(err)
				check.Zero(lookups)
				refs, err = f.store.MessageMatrixAttachments(ids["$text"])
				req.NoError(err)
				check.Empty(refs)
			}
		})
	}
}

func TestImportMediaCryptoFailuresAreFailed(t *testing.T) {
	for _, failure := range []string{"hash", "key", "version"} {
		t.Run(failure, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			ciphertext := []byte("encrypted file")
			file := matrixattachment.NewEncryptedFile()
			file.EncryptInPlace(ciphertext)
			switch failure {
			case "hash":
				file.Hashes.SHA256 = strings.Repeat("A", 43)
			case "key":
				file.Key.Key = "invalid"
			case "version":
				file.Version = "v3"
			}
			content := &event.MessageEventContent{MsgType: event.MsgFile, Body: "file", File: &event.EncryptedFileInfo{EncryptedFile: *file, URL: "mxc://example.org/file"}}
			raw, err := json.Marshal(map[string]any{"type": "m.room.message", "event_id": "$media", "sender": "@member:example.org", "content": content})
			req.NoError(err)
			f.timeline = []*event.Event{matrixTestEvent(t, string(raw))}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			requests := 0
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write(ciphertext)
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(int64(1), sum.AttachmentsFailed)
			check.Zero(sum.AttachmentsPending)
			if failure == "hash" {
				check.Equal(1, requests)
			} else {
				check.Zero(requests)
			}
		})
	}
}

func TestImportMediaPublicationInterruptionPreservesStoredAttachment(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	requests := 0
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("archived bytes"))
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	opts.MediaMutation = func(_ context.Context, write func() error) error {
		if err := write(); err != nil {
			return err
		}
		if requests > 0 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	_, err := NewImporter(f.store, runtime).Import(ctx, opts)
	req.ErrorIs(err, context.Canceled)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	for _, quiet := range []bool{false, true} {
		if quiet {
			f.quiet = true
			opts.MediaMutation = nil
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
		}
		var count int64
		var hasAttachments bool
		req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT attachment_count, has_attachments FROM messages WHERE id = ?`), ids["$media"]).Scan(&count, &hasAttachments))
		check.Equal(int64(1), count)
		check.True(hasAttachments)
		refs, err := f.store.MessageMatrixAttachments(ids["$media"])
		req.NoError(err)
		ref := refs["matrix:mxc://example.org/file"]
		check.Equal(attachmentpolicy.StateStored, ref.State)
		data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, ref.StoragePath))
		req.NoError(err)
		check.Equal("archived bytes", string(data))
	}
	check.Equal(1, requests)
}

func TestImportFullSyncBackfillsRecoveredMedia(t *testing.T) {
	for _, selection := range []string{"original", "edited", "text-edit", "sticker"} {
		t.Run(selection, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			typ := event.EventMessage
			content := map[string]any{"msgtype": "m.image", "body": "photo.jpg", "url": "mxc://example.org/original"}
			if selection == "sticker" {
				typ = event.EventSticker
				delete(content, "msgtype")
			}
			f.timeline = []*event.Event{f.backup.encrypted(t, "$photo", 10, typ, content)}
			if selection == "edited" || selection == "text-edit" {
				msgType := "m.image"
				if selection == "text-edit" {
					msgType = "m.text"
				}
				f.timeline = append(f.timeline, f.backup.encrypted(t, "$edit", 20, typ, map[string]any{
					"msgtype": msgType, "body": "* selected",
					"m.new_content": map[string]any{"msgtype": msgType, "body": "selected", "url": "mxc://example.org/selected"},
					"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": "$photo"},
				}))
			}
			sum, err := f.run(t, true, false)
			req.NoError(err)
			req.Equal(int64(1), sum.Recovered)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$photo"})
			req.NoError(err)
			messageID := ids["$photo"]
			req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), messageID, nil))
			body := f.body(t, "$photo")
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			var downloads []string
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/{media}", func(w http.ResponseWriter, r *http.Request) {
				downloads = append(downloads, r.PathValue("media"))
				_, _ = w.Write([]byte("photo bytes"))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			runtime.key = nil
			opts := ImportOptions{UserID: "@archive:example.org", Full: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Zero(sum.Recovered)
			check.Equal(body, f.body(t, "$photo"))
			refs, err := f.store.MessageMatrixAttachments(messageID)
			req.NoError(err)
			if selection == "text-edit" {
				check.Empty(refs)
				check.Empty(downloads)
				return
			}
			want := "original"
			if selection == "edited" {
				want = "selected"
			}
			check.Equal([]string{want}, downloads)
			check.Equal(int64(1), sum.AttachmentsDownloaded)
			req.Len(refs, 1)
			ref := refs["matrix:mxc://example.org/"+want]
			check.Equal(attachmentpolicy.StateStored, ref.State)
			data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, ref.StoragePath))
			req.NoError(err)
			check.Equal("photo bytes", string(data))
		})
	}
}

func makeMediaRetryDue(t *testing.T, s *store.Store, messageID int64) {
	t.Helper()
	refs, err := s.MessageMatrixAttachments(messageID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	for _, ref := range refs {
		var metadata matrixMediaMetadata
		require.NoError(t, json.Unmarshal([]byte(ref.Metadata), &metadata))
		metadata.RetryAfter = time.Now().Add(-time.Second)
		encoded, err := json.Marshal(metadata)
		require.NoError(t, err)
		ref.Metadata = string(encoded)
		require.NoError(t, s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{ref}))
	}
}

func TestImportLegacyMediaDiscoveryResumesBudget(t *testing.T) {
	for _, scenario := range []string{"complete", "temporary", "scope", "full"} {
		t.Run(scenario, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			room, err := f.store.EnsureConversationWithType(f.sourceID, "!room:example.org", "direct_chat", "Legacy")
			req.NoError(err)
			var ids []int64
			tx, err := f.store.DB().Begin()
			req.NoError(err)
			defer func() { _ = tx.Rollback() }()
			for i := range legacyMediaDiscoveryBudget + 1 {
				eventID := fmt.Sprintf("$legacy%d", i)
				var messageID int64
				req.NoError(tx.QueryRow(f.store.Rebind(`INSERT INTO messages (source_id, conversation_id, source_message_id, message_type) VALUES (?, ?, ?, ?) RETURNING id`), f.sourceID, room, eventID, SourceType).Scan(&messageID))
				ids = append(ids, messageID)
				raw := fmt.Sprintf(`{"type":"m.room.message","event_id":%q,"sender":"@member:example.org","content":{"msgtype":"m.text","body":"legacy"}}`, eventID)
				_, err := tx.Exec(f.store.Rebind(`INSERT INTO message_raw (message_id, raw_data, raw_format, compression) VALUES (?, ?, ?, 'none')`), messageID, []byte(raw), rawFormat)
				req.NoError(err)
			}
			req.NoError(tx.Commit())
			if scenario == "temporary" {
				req.NoError(f.store.SetMessageMetadata(ids[0], sql.NullString{String: `{"matrix_edit_event_id":"$edit","matrix_edit_ts":20}`, Valid: true}))
			}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			unavailable, lookups := true, 0
			mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/event/$edit", func(w http.ResponseWriter, _ *http.Request) {
				lookups++
				if unavailable {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeMatrixJSON(t, w, matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"},"m.relates_to":{"rel_type":"m.replace","event_id":"$legacy0"}}}`))
			})
			f.quiet = true
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true}
			run := func() *SyncState {
				_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.NoError(err)
				previous, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
				req.NoError(err)
				state, err := loadSyncState(previous.CursorAfter.String)
				req.NoError(err)
				return state
			}
			state := run()
			check.Zero(state.MediaDiscoveryVersion)
			check.Equal(ids[legacyMediaDiscoveryBudget-1], state.MediaDiscoveryAfter)
			check.Equal(scenario == "temporary", state.MediaDiscoveryIncomplete)
			unavailable = false
			if scenario == "scope" {
				opts.ExcludeRooms = []string{"!other:example.org"}
			}
			opts.Full = scenario == "full"
			state = run()
			opts.Full = false
			if scenario == "complete" {
				check.Equal(mediaDiscoveryVersion, state.MediaDiscoveryVersion)
				check.Zero(state.MediaDiscoveryAfter)
				return
			}
			check.Zero(state.MediaDiscoveryVersion)
			if scenario == "temporary" {
				check.Zero(state.MediaDiscoveryAfter)
				check.False(state.MediaDiscoveryIncomplete)
				check.Equal(1, lookups, "finish the pass before retrying the earlier lookup")
			} else {
				check.Equal(ids[legacyMediaDiscoveryBudget-1], state.MediaDiscoveryAfter)
			}
			state = run()
			if scenario == "temporary" {
				check.Zero(state.MediaDiscoveryVersion)
				state = run()
			}
			check.Equal(mediaDiscoveryVersion, state.MediaDiscoveryVersion)
			check.Zero(state.MediaDiscoveryAfter)
			if scenario == "temporary" {
				check.Equal(2, lookups)
				refs, err := f.store.MessageMatrixAttachments(ids[0])
				req.NoError(err)
				check.Equal(attachmentpolicy.StatePending, refs["matrix:mxc://example.org/file"].State)
			}
		})
	}
}

func TestImportLegacyMediaDiscoverySkipsMalformedEvents(t *testing.T) {
	for _, raw := range []string{`malformed`, `{"type":"m.room.message","event_id":"$media","content":{"msgtype":42}}`} {
		t.Run(raw, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.text","body":"text"}}`)}
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			req.NoError(f.store.UpsertMessageRawWithFormat(ids["$media"], []byte(raw), rawFormat))
			f.quiet, f.left = true, true
			opts.Rooms = []string{"!room:example.org"}
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			previous, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			state, err := loadSyncState(previous.CursorAfter.String)
			req.NoError(err)
			check.Equal(mediaDiscoveryVersion, state.MediaDiscoveryVersion)
			check.Zero(state.MediaDiscoveryAfter)
			check.False(state.MediaDiscoveryIncomplete)
			_, err = f.store.DB().Exec(f.store.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), ids["$media"])
			req.NoError(err)
			_, _, _, err = NewImporter(f.store, runtime).archivedMediaContent(ids["$media"])
			req.ErrorIs(err, errReadArchivedEvent)
		})
	}
}

func TestImportLegacyMediaDiscoveryRetriesIncompleteSync(t *testing.T) {
	for _, scenario := range []string{"transient-edit", "forbidden-edit", "missing-edit", "malformed-edit", "wrong-sender", "wrong-target", "missing-content", "unsupported-encryption"} {
		t.Run(scenario, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"original","url":"mxc://example.org/original"}}`)}
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			messageID := ids["$media"]
			req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), messageID, nil))
			req.NoError(f.store.SetMessageMetadata(messageID, sql.NullString{String: `{"matrix_edit_event_id":"$edit","matrix_edit_ts":20}`, Valid: true}))
			previous, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			state, err := loadSyncState(previous.CursorAfter.String)
			req.NoError(err)
			state.MediaDiscoveryVersion = 0
			blob, err := state.marshal()
			req.NoError(err)
			syncID, err := f.store.StartSync(f.sourceID, SourceType)
			req.NoError(err)
			req.NoError(f.store.CompleteSyncAndUpdateSourceCursor(syncID, f.sourceID, blob))
			f.quiet, f.left, f.historyFails = true, true, true
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			unavailable := scenario == "transient-edit"
			lookups, downloads := 0, 0
			mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/event/$edit", func(w http.ResponseWriter, _ *http.Request) {
				lookups++
				switch scenario {
				case "malformed-edit":
					writeMatrixJSON(t, w, map[string]any{"type": "m.room.message", "content": map[string]any{"msgtype": 123}})
					return
				case "wrong-sender", "wrong-target", "missing-content":
					sender, target := "@member:example.org", "$media"
					content := map[string]any{"m.new_content": map[string]any{"msgtype": "m.file", "body": "selected", "url": "mxc://example.org/selected"}}
					switch scenario {
					case "wrong-sender":
						sender = "@other:example.org"
					case "wrong-target":
						target = "$other"
					case "missing-content":
						delete(content, "m.new_content")
					}
					content["m.relates_to"] = map[string]any{"rel_type": "m.replace", "event_id": target}
					writeMatrixJSON(t, w, map[string]any{"type": "m.room.message", "event_id": "$edit", "sender": sender, "content": content})
					return
				case "unsupported-encryption":
					writeMatrixJSON(t, w, map[string]any{"type": "m.room.encrypted", "content": map[string]any{"algorithm": "unsupported"}})
					return
				}
				if scenario == "forbidden-edit" || scenario == "missing-edit" {
					status := http.StatusForbidden
					if scenario == "missing-edit" {
						status = http.StatusNotFound
					}
					w.WriteHeader(status)
					return
				}
				if unavailable {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeMatrixJSON(t, w, matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.file","body":"selected","url":"mxc://example.org/selected"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`))
			})
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/selected", func(w http.ResponseWriter, _ *http.Request) {
				downloads++
				_, _ = w.Write([]byte("selected bytes"))
			})
			opts.NoMedia = false
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Zero(downloads)
			previous, err = f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			state, err = loadSyncState(previous.CursorAfter.String)
			req.NoError(err)
			if scenario == "transient-edit" {
				check.Zero(state.MediaDiscoveryVersion)
			} else {
				check.Equal(mediaDiscoveryVersion, state.MediaDiscoveryVersion)
			}
			if scenario != "transient-edit" {
				_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
				req.NoError(err)
				check.Equal(1, lookups)
				return
			}
			unavailable = false
			sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(int64(1), sum.AttachmentsDownloaded)
			check.Equal(1, downloads)
			refs, err := f.store.MessageMatrixAttachments(messageID)
			req.NoError(err)
			check.Equal(attachmentpolicy.StateStored, refs["matrix:mxc://example.org/selected"].State)
			previous, err = f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			state, err = loadSyncState(previous.CursorAfter.String)
			req.NoError(err)
			check.Equal(mediaDiscoveryVersion, state.MediaDiscoveryVersion)
			if scenario == "transient-edit" {
				check.GreaterOrEqual(lookups, 2)
			}
		})
	}
}

func TestImportFullDiscoversLegacyMediaWithoutSourceHistory(t *testing.T) {
	for _, scenario := range []string{"joined-without-saved-content", "shrunken-roster", "redacted", "left", "left-edited", "left-text-edited"} {
		t.Run(scenario, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/original"}}`)}
			want := "original"
			if scenario == "left-edited" || scenario == "left-text-edited" {
				f.timeline = append(f.timeline, matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"msgtype":"m.file","body":"* selected","m.new_content":{"msgtype":"m.file","body":"selected","url":"mxc://example.org/selected"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`))
				want = "selected"
				if scenario == "left-text-edited" {
					f.timeline[1] = matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"m.new_content":{"msgtype":"m.text","body":"selected text"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)
				}
			}
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			var editLookups int
			if scenario == "left-edited" || scenario == "left-text-edited" {
				edit := f.timeline[1]
				mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/event/$edit", func(w http.ResponseWriter, _ *http.Request) {
					editLookups++
					writeMatrixJSON(t, w, edit)
				})
			}
			var downloads []string
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/{file}", func(w http.ResponseWriter, r *http.Request) {
				downloads = append(downloads, r.PathValue("file"))
				_, _ = w.Write([]byte("archived bytes"))
			})
			runtime := backupRuntime(t, f.server.URL, f.backup)
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
			_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
			req.NoError(err)
			req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), ids["$media"], nil))
			if scenario == "left-edited" || scenario == "left-text-edited" {
				req.NoError(f.store.SetMessageMetadata(ids["$media"], sql.NullString{String: `{"matrix_edit_event_id":"$edit","matrix_edit_ts":20}`, Valid: true}))
			}
			switch scenario {
			case "joined-without-saved-content":
				metadata, err := NewImporter(f.store, runtime).mergedMetadata(ids["$media"], map[string]any{"matrix_media_content": nil})
				req.NoError(err)
				req.NoError(f.store.SetMessageMetadata(ids["$media"], sql.NullString{String: string(metadata), Valid: true}))
			case "shrunken-roster":
				_, err = f.store.DB().Exec(f.store.Rebind(`UPDATE conversations SET participant_count = 30, metadata = ? WHERE source_id = ?`), `{"member_count":30}`, f.sourceID)
				req.NoError(err)
				f.timeline = nil
				opts.MediaPolicy.MaxParticipants = 20
			case "redacted":
				f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redact","content":{}}},"content":{}}`)}
			default:
				f.quiet, f.left, f.historyFails = true, true, true
			}
			opts.Full = true
			sum, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			if scenario == "left-text-edited" {
				check.Zero(sum.AttachmentsPending)
				metadata, err := f.store.GetMessageMetadata(ids["$media"])
				req.NoError(err)
				var values map[string]any
				req.NoError(json.Unmarshal([]byte(metadata.String), &values))
				check.Contains(values, "matrix_media_content")
				check.Contains(values, "matrix_media_content_version")
				check.Empty(downloads)
				check.Equal(1, editLookups)
				return
			}
			check.Equal(int64(1), sum.AttachmentsPending)
			check.Empty(downloads)
			if scenario == "shrunken-roster" {
				refs, err := f.store.MessageMatrixAttachments(ids["$media"])
				req.NoError(err)
				check.Equal(attachmentpolicy.StatePending, refs["matrix:mxc://example.org/original"].State)
			}
			opts.NoMedia = false
			opts.MediaPolicy.Scope = attachmentpolicy.ScopeNone
			_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Empty(downloads)
			opts.MediaPolicy.Scope = attachmentpolicy.ScopeAll
			sum, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal([]string{want}, downloads)
			if scenario == "joined-without-saved-content" {
				check.Zero(sum.MessagesAdded)
				check.Equal("[file: file]", f.body(t, "$media"))
			}
			check.Equal(int64(1), sum.AttachmentsDownloaded)
			refs, err := f.store.MessageMatrixAttachments(ids["$media"])
			req.NoError(err)
			check.Equal(attachmentpolicy.StateStored, refs["matrix:mxc://example.org/"+want].State)
			if scenario == "joined-without-saved-content" {
				check.Equal("file", refs["matrix:mxc://example.org/"+want].Filename)
			}
			if scenario == "left-edited" || scenario == "left-text-edited" {
				check.Equal(1, editLookups)
			}
			data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs["matrix:mxc://example.org/"+want].StoragePath))
			req.NoError(err)
			check.Equal("archived bytes", string(data))
		})
	}
}

func TestImportFullDirectMediaScopeUsesCurrentDirectMapForLeftRoom(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	direct := true
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		if direct {
			_, _ = w.Write([]byte(`{"@member:example.org":["!room:example.org"]}`))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	})
	var downloads int
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/file", func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		_, _ = w.Write([]byte("file bytes"))
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeDirect}}
	_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(ids["$media"])
	req.NoError(err)
	check.Equal(attachmentpolicy.StatePending, refs["matrix:mxc://example.org/file"].State)
	direct, f.quiet, f.left, f.historyFails = false, true, true, true
	opts.NoMedia = false
	for _, full := range []bool{true, false} {
		opts.Full = full
		_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
		req.NoError(err)
		check.Zero(downloads)
		refs, err = f.store.MessageMatrixAttachments(ids["$media"])
		req.NoError(err)
		check.Equal(attachmentpolicy.StateSkipped, refs["matrix:mxc://example.org/file"].State)
		check.Equal(attachmentpolicy.SkipPolicyScope, refs["matrix:mxc://example.org/file"].SkipReason)
	}
	direct = true
	_, err = NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(1, downloads)
}

func TestImportDirectMediaScopeExcludesNonDirectRooms(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	media := func(name string) *event.Event {
		return matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.message","event_id":"$%s","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/%s"}}`, name, name))
	}
	f.timeline = []*event.Event{media("direct")}
	f.extraRooms = map[string][]*event.Event{"!public:example.org": {media("public")}}
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!public:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{},"@member:example.org":{},"@third:example.org":{}}}`))
	})
	var downloads []string
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/{file}", func(w http.ResponseWriter, r *http.Request) {
		downloads = append(downloads, r.PathValue("file"))
		_, _ = w.Write([]byte("file"))
	})
	runtime := backupRuntime(t, f.server.URL, f.backup)
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeDirect}}
	_, err := NewImporter(f.store, runtime).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal([]string{"direct"}, downloads)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$public"})
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(ids["$public"])
	req.NoError(err)
	check.Equal(attachmentpolicy.SkipPolicyScope, refs["matrix:mxc://example.org/public"].SkipReason)
	var kind string
	req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT c.conversation_type FROM conversations c JOIN messages m ON m.conversation_id = c.id WHERE m.id = ?`), ids["$public"]).Scan(&kind))
	check.Equal("group_chat", kind)
}
