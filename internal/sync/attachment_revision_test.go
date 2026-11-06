package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func revisionHTMLMIME() []byte {
	raw := testemail.NewMessage().Body(`<img src="http://images.example/receipt">`).
		WithAttachment("required.bin", "application/octet-stream", []byte("Hello World!")).Bytes()
	return []byte(strings.Replace(string(raw), "Content-Type: text/plain", "Content-Type: text/html", 1))
}

func revisionImageFetcher(dial func(context.Context, string, string) (net.Conn, error)) *remoteimage.Fetcher {
	f := remoteimage.NewFetcher()
	f.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	f.DialContext = dial
	return f
}

func revisionImageServer(t *testing.T) (*remoteimage.Fetcher, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic"))
	}))
	t.Cleanup(server.Close)
	return revisionImageFetcher(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
	}), &requests
}

func revisionStoreLocation(t *testing.T, st *store.Store) string {
	t.Helper()
	require := require.New(t)
	if st.IsPostgreSQL() {
		var database, schema string
		require.NoError(st.DB().QueryRow("SELECT current_database(), current_schema()").Scan(&database, &schema))
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(err)
		u.Path = "/" + database
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	var sequence int
	var name, location string
	require.NoError(st.DB().QueryRow("PRAGMA database_list").Scan(&sequence, &name, &location))
	return location
}

func TestAttachmentRevisionCrashWorker(t *testing.T) {
	mode := os.Getenv("MSGVAULT_ATTACHMENT_CRASH_MODE")
	if mode == "" {
		t.Skip("subprocess worker")
	}
	require := require.New(t)
	st, err := store.OpenForTest(os.Getenv("MSGVAULT_ATTACHMENT_CRASH_DSN"))
	require.NoError(err)
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: testEmail, HistoryID: 1000}
	raw := testMIMEWithAttachment()
	opts := &Options{AttachmentsDir: os.Getenv("MSGVAULT_ATTACHMENT_CRASH_DIR")}
	if mode == "before-image" {
		raw = revisionHTMLMIME()
		opts.RemoteImages = revisionImageFetcher(func(context.Context, string, string) (net.Conn, error) {
			os.Exit(74) // Abrupt termination after MIME publication, before the first image response.
			return nil, errors.New("unreachable")
		})
	}
	mock.AddMessage("interrupted", raw, []string{"INBOX"})
	logger := newSyncLogCapture(func(record slog.Record) {
		if mode == "mime-error" && record.Message == "failed to ingest message" {
			os.Exit(74)
		}
	})
	_, err = New(mock, st, opts).WithLogger(logger.logger()).Full(t.Context(), testEmail)
	require.FailNow("worker did not reach its crash boundary", "error=%v", err)
}

func TestAttachmentRevisionCrashRetainsDurableWork(t *testing.T) {
	for _, mode := range []string{"mime-error", "before-image"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			source := e.CreateSourceWithHistory(t, "1000")
			dir := filepath.Join(e.TmpDir, "attachments")
			if mode == "mime-error" {
				require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
			}
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAttachmentRevisionCrashWorker$", "-test.timeout=1m") //nolint:gosec // Reexecutes the test binary against this synthetic fixture.
			child.Env = append(os.Environ(), "MSGVAULT_ATTACHMENT_CRASH_MODE="+mode,
				"MSGVAULT_ATTACHMENT_CRASH_DSN="+revisionStoreLocation(t, e.Store), "MSGVAULT_ATTACHMENT_CRASH_DIR="+dir)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(err, &exit, "%s", output)
			require.Equal(74, exit.ExitCode(), "%s", output)
			reopenAcquisitionStore(t, e)
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"interrupted"})
			require.NoError(err)
			require.Contains(ids, "interrupted")
			raw, err := e.Store.GetMessageRaw(ids["interrupted"])
			require.NoError(err)
			expected := testMIMEWithAttachment()
			if mode == "before-image" {
				expected = revisionHTMLMIME()
			}
			assert.Equal(expected, raw)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			require.Len(debt, 1, "committed RAW must carry a durable obligation after process exit")
			if mode == "mime-error" {
				require.NoError(os.Remove(dir))
			}
			opts := &Options{AttachmentsDir: dir}
			var requests *atomic.Int64
			if mode == "before-image" {
				opts.RemoteImages, requests = revisionImageServer(t)
			}
			e.Syncer = New(e.Mock, e.Store, opts)
			sum, err := e.Syncer.Incremental(t.Context(), source)
			require.NoError(err)
			assert.Zero(sum.Errors)
			assert.Empty(e.Mock.GetMessageCalls, "recovery must use retained content")
			refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), ids["interrupted"])
			require.NoError(err)
			require.Len(refs, 1)
			bytes, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
			require.NoError(err)
			assert.Equal([]byte("Hello World!"), bytes)
			if requests != nil {
				assert.Equal(int64(1), requests.Load())
			}
			debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			assert.Empty(debt)
		})
	}
}

func TestAttachmentRevisionDebtWriteFailureRollsBackRAW(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	dir := filepath.Join(e.TmpDir, "blocked")
	require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
	e.Syncer = New(e.Mock, e.Store, &Options{AttachmentsDir: dir})
	e.Mock.AddMessage("atomic-debt", testMIMEWithAttachment(), []string{"INBOX"})
	allow := acquisitionWriteFault(t, e, "sync_run_items", "INSERT", "NEW.status = 'error'")
	_, err := e.Syncer.Full(t.Context(), testEmail)
	require.Error(err)
	reopenAcquisitionStore(t, e)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"atomic-debt"})
	require.NoError(err)
	assert.Empty(ids, "a rejected pending marker must roll back its RAW transaction")
	allow()
}

func TestAttachmentRevisionUppercaseHash(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "keyed", true: "legacy"}[legacy], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			dir := filepath.Join(e.TmpDir, "attachments")
			e.Mock.AddMessage("uppercase", testMIMEWithAttachment(), []string{"INBOX"})
			_, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
			require.NoError(err)
			source := e.CreateSource(t)
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"uppercase"})
			require.NoError(err)
			id := ids["uppercase"]
			query := `UPDATE attachments SET content_hash = UPPER(content_hash)`
			if legacy {
				query += `, source_part_key = NULL`
			}
			_, err = e.Store.DB().Exec(e.Store.Rebind(query+` WHERE message_id = ?`), id)
			require.NoError(err)
			before, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			require.Len(before, 1)
			path := filepath.Join(dir, before[0].StoragePath)
			info, err := os.Stat(path)
			require.NoError(err)
			service := New(nil, e.Store, &Options{AttachmentsDir: dir})
			for range 2 {
				require.NoError(service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "uppercase", SourceID: source.ID}))
			}
			after, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			require.Len(after, 1)
			assert.Equal(before[0].ContentHash, after[0].ContentHash)
			assert.NotEmpty(after[0].SourcePartKey)
			got, err := os.Stat(path)
			require.NoError(err)
			assert.True(os.SameFile(info, got))
			assert.Equal(info.ModTime(), got.ModTime())
		})
	}
}

type revisionRepeatedLabelAPI struct {
	*gmail.MockAPI

	calls int
}

func (a *revisionRepeatedLabelAPI) GetMessageRaw(ctx context.Context, id string) (*gmail.RawMessage, error) {
	a.calls++
	if a.calls > 1 {
		return nil, &gmail.NotFoundError{Path: "/messages/" + id}
	}
	return a.MockAPI.GetMessageRaw(ctx, id)
}

func TestAttachmentRevisionRepeatedLabelDoesNotEraseDebt(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	dir := filepath.Join(e.TmpDir, "blocked")
	require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
	e.Mock.AddMessage("repeated", testMIMEWithAttachment(), []string{"INBOX"})
	e.SetHistory(1001, historyLabelAdded("repeated", "INBOX"), historyLabelAdded("repeated", "STARRED"))
	api := &revisionRepeatedLabelAPI{MockAPI: e.Mock}
	e.Syncer = New(api, e.Store, &Options{AttachmentsDir: dir})
	sum, err := e.Syncer.Incremental(t.Context(), source)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors)
	assert.Equal(1, api.calls)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	assert.Equal(syncItemKindAttachmentError, debt[0].ErrorKind)
	require.NoError(os.Remove(dir))
	e.Mock.GetMessageCalls = nil
	e.SetHistory(1001)
	source, err = e.Store.GetSourceByID(source.ID)
	require.NoError(err)
	sum, err = New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Incremental(t.Context(), source)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Empty(e.Mock.GetMessageCalls)
}

func TestAttachmentRevisionReferenceDiagnostics(t *testing.T) {
	require := require.New(t)
	e := attachmentPersistenceEnv(t)
	service := New(nil, e.Store, &Options{AttachmentsDir: t.TempDir()})
	err := service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "unknown"})
	require.EqualError(err, "No matching message")
	for _, email := range []string{"first@example.com", "second@example.com"} {
		source, err := e.Store.GetOrCreateSource("gmail", email)
		require.NoError(err)
		seedRepairRow(t, e.Store, source.ID, "ambiguous", "thread", "subject")
	}
	err = service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "ambiguous"})
	require.EqualError(err, "Multiple messages match; specify --source-id")
}

func TestAttachmentRevisionStaleOwnerCannotSettleDebt(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSource(t)
	oldID, err := e.Store.StartSync(source.ID, "incremental")
	require.NoError(err)
	stale := New(nil, e.Store.ScopedToSync(source.ID, oldID), nil)
	stale.setAcquisitionReplay(oldID, []string{"pending"})
	require.NoError(e.Store.FailSync(oldID, "superseded synthetic owner"))
	newID, err := e.Store.StartSync(source.ID, "incremental")
	require.NoError(err)
	require.NoError(e.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: newID, SourceMessageID: "pending", Phase: syncItemPhaseIngest,
		Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindAttachmentError, ErrorMessage: "still pending"}))
	require.ErrorIs(stale.resolveAcquisition("pending"), store.ErrSyncRunSuperseded)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	assert.Equal(newID, debt[0].SyncRunID)
}

func TestAttachmentRevisionUnrelatedOutcomeCannotEraseDebt(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSource(t)
	runID, err := e.Store.StartSync(source.ID, "incremental")
	require.NoError(err)
	service := New(nil, e.Store.ScopedToSync(source.ID, runID), nil)
	service.setAcquisitionReplay(runID, nil)
	require.NoError(service.recordAcquisitionFailure(runID, "pending", syncItemPhaseIngest, syncItemKindIngestError, errAttachmentMaterialization))
	service.recordSyncItem(runID, "pending", syncItemPhaseFetch, store.SyncRunItemStatusSkipped, syncItemKindGmailNotFound, &gmail.NotFoundError{Path: "/messages/pending"})
	require.NoError(service.recordAcquisitionFailure(runID, "pending", syncItemPhaseFetch, syncItemKindFetchError, errors.New("unrelated fetch failure")))
	require.Error(service.resolveAcquisition("pending"), "generic acquisition success must not settle unfinished materialization")
	require.NoError(e.Store.CompleteSync(runID, "1001"))
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	assert.Equal(syncItemKindAttachmentError, debt[0].ErrorKind)
}

func TestAttachmentRevisionHTMLWarningIsRepairable(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	raw := revisionHTMLMIME()
	parsed, err := mime.Parse(raw)
	require.NoError(err)
	require.NotEmpty(parsed.Errors, "HTML-only mail reports a harmless text conversion warning")
	e.Mock.AddMessage("html-only", raw, []string{"INBOX"})
	_, err = New(e.Mock, e.Store, &Options{}).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	err = New(nil, e.Store, &Options{AttachmentsDir: filepath.Join(e.TmpDir, "attachments")}).RepairMessageAttachments(t.Context(), RepairRequest{Reference: "html-only", SourceID: source.ID})
	assert.NoError(err)
}

func TestAttachmentRevisionIncompleteExtractionKeepsDebt(t *testing.T) {
	for name, raw := range map[string][]byte{
		"invalid transfer": []byte(strings.Replace(string(testMIMEWithAttachment()), "SGVsbG8gV29ybGQh", "S", 1)),
		"fatal envelope":   []byte("not a MIME message\x00"),
	} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			parsed, err := mime.Parse(raw)
			require.True(err != nil || strings.Contains(strings.Join(parsed.Errors, "\n"), "[E]"), "the fixture must actually lose MIME content")
			e.Mock.AddMessage("incomplete", raw, []string{"INBOX"})
			dir := filepath.Join(e.TmpDir, "attachments")
			sum, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
			require.NoError(err)
			assert.Equal(int64(1), sum.Errors)
			source := e.CreateSource(t)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			assert.Len(debt, 1)
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"incomplete"})
			require.NoError(err)
			require.Contains(ids, "incomplete")
			retained, err := e.Store.GetMessageRaw(ids["incomplete"])
			require.NoError(err)
			assert.Equal(raw, retained)
		})
	}
}

func TestAttachmentRevisionSettlementWriteFailureKeepsDebt(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	dir := filepath.Join(e.TmpDir, "attachments")
	e.Mock.AddMessage("settlement", testMIMEWithAttachment(), []string{"INBOX"})
	allow := acquisitionWriteFault(t, e, "sync_run_items", "INSERT", "NEW.status = 'resolved'")
	sum, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors)
	reopenAcquisitionStore(t, e)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	allow()
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"settlement"})
	require.NoError(err)
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), ids["settlement"])
	require.NoError(err)
	require.Len(refs, 1)
	path := filepath.Join(dir, refs[0].StoragePath)
	before, err := os.Stat(path)
	require.NoError(err)
	e.Mock.GetMessageCalls = nil
	sum, err = New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Incremental(t.Context(), source)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Empty(e.Mock.GetMessageCalls)
	after, err := os.Stat(path)
	require.NoError(err)
	assert.True(os.SameFile(before, after))
	assert.Equal(before.ModTime(), after.ModTime())
}

func TestAttachmentRevisionRemoteFailureRemainsPending(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	dir := filepath.Join(e.TmpDir, "attachments")
	require.NoError(os.MkdirAll(dir, 0700))
	hash := sha256.Sum256([]byte("Hello World!"))
	blocked := filepath.Join(dir, hex.EncodeToString(hash[:])[:2])
	require.NoError(os.WriteFile(blocked, []byte("blocked MIME prefix"), 0600))
	fetcher := revisionImageFetcher(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("synthetic image network failure")
	})
	e.Mock.AddMessage("both", revisionHTMLMIME(), []string{"INBOX"})
	sum, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir, RemoteImages: fetcher}).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	assert.Contains(debt[0].ErrorMessage, "save attachments")
	assert.Contains(debt[0].ErrorMessage, "archive remote images")
	require.NoError(os.Remove(blocked))
	reopenAcquisitionStore(t, e)
	e.Mock.GetMessageCalls = nil
	sum, err = New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Incremental(t.Context(), source)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors, "disabling image downloads must retain pending image work")
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"both"})
	require.NoError(err)
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), ids["both"])
	require.NoError(err)
	require.Len(refs, 1)
	path := filepath.Join(dir, refs[0].StoragePath)
	info, err := os.Stat(path)
	require.NoError(err)
	fetcher, requests := revisionImageServer(t)
	sum, err = New(e.Mock, e.Store, &Options{AttachmentsDir: dir, RemoteImages: fetcher}).Incremental(t.Context(), source)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Equal(int64(1), requests.Load())
	assert.Empty(e.Mock.GetMessageCalls)
	after, err := os.Stat(path)
	require.NoError(err)
	assert.True(os.SameFile(info, after))
	assert.Equal(info.ModTime(), after.ModTime())
	debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(debt)
}

func TestAttachmentRevisionExistingMIMEDebtAlsoArchivesImages(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	dir := filepath.Join(e.TmpDir, "attachments")
	require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
	e.Mock.AddMessage("legacy-images", revisionHTMLMIME(), []string{"INBOX"})
	_, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	require.Equal(syncItemKindAttachmentError, debt[0].ErrorKind)
	require.NoError(os.Remove(dir))
	fetcher, requests := revisionImageServer(t)
	e.Mock.GetMessageCalls = nil
	sum, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir, RemoteImages: fetcher}).Incremental(t.Context(), source)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Equal(int64(1), requests.Load())
	assert.Empty(e.Mock.GetMessageCalls)
}
