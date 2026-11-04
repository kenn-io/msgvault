package sync

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/testutil"
)

func mimeNULEnv(t *testing.T) *TestEnv {
	t.Helper()
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: testEmail, HistoryID: 1001, MessagesTotal: 1}
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.AttachmentsDir = filepath.Join(t.TempDir(), "attachments")
	return &TestEnv{Store: st, Mock: mock, Syncer: New(mock, st, opts), Context: t.Context()}
}

func TestMIMENULAcquisition(t *testing.T) {
	for _, route := range []string{"full", "full_identities", "history", "labels", "replay", "recovery", "repair"} {
		t.Run(route, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			e := mimeNULEnv(t)
			source := e.CreateSource(t)
			if route == "replay" {
				source = seedReplaySource(t, e, "nul")
			}
			raw, err := os.ReadFile("../mime/testdata/nul.eml")
			require.NoError(err)
			if route == "full_identities" {
				raw = bytes.ReplaceAll(raw, []byte("<nul@example.org>"), []byte("<nul\x00@example.org>"))
				raw = append([]byte("In-Reply-To: <bad\x00@example.org>\r\nList-ID: <bad\x00.example.org>\r\n"), raw...)
			}
			e.Mock.AddMessage("nul", raw, []string{"INBOX"})
			e.Mock.Messages["nul"].Snippet = "preview\x00tail"
			var summary *gmail.SyncSummary
			switch route {
			case "full", "full_identities":
				summary, err = e.Syncer.Full(t.Context(), testEmail)
			case "repair":
				seedRepairRow(t, e.Store, source.ID, "nul", "old-thread", "old")
				_, err = e.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "nul", SourceID: source.ID})
			default:
				if route != "replay" {
					require.NoError(e.Store.UpdateSourceSyncCursor(source.ID, "1000"))
					source.SyncCursor = sql.NullString{String: "1000", Valid: true}
				}
				switch route {
				case "history":
					e.SetHistory(1001, historyAdded("nul"))
				case "labels":
					e.SetHistory(1001, historyLabelAdded("nul", "INBOX"))
				}
				if route == "recovery" {
					summary, err = e.Syncer.RecoverExpiredHistory(t.Context(), source)
				} else {
					summary, err = e.Syncer.Incremental(t.Context(), source)
				}
			}
			require.NoError(err)
			if summary != nil {
				require.Zero(summary.Errors)
			}
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"nul"})
			require.NoError(err)
			require.Contains(ids, "nul")
			id := ids["nul"]
			stored, err := e.Store.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(raw, stored)
			body, err := e.Store.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal("beforetoken�aftertoken", body)
			message, err := e.Store.GetMessage(id)
			require.NoError(err)
			assert.Equal("Report�tail", message.Subject)
			assert.Equal("preview�tail", message.Snippet)
			for _, term := range []string{"beforetoken", "aftertoken"} {
				_, total, searchErr := e.Store.SearchMessages(term, 0, 10)
				require.NoError(searchErr)
				assert.Equal(int64(1), total)
			}
			var filename, name string
			require.NoError(e.Store.DB().QueryRow(e.Store.Rebind(`SELECT filename FROM attachments WHERE message_id = ?`), id).Scan(&filename))
			assert.Equal("file�name.bin", filename)
			require.NoError(e.Store.DB().QueryRow(e.Store.Rebind(`SELECT display_name FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`), id).Scan(&name))
			assert.Equal("Sender�Name", name)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			assert.Empty(debt)
			assert.Empty(collectAudit(t, e.Syncer, source.ID), "audit and ingestion must derive the same projections")
		})
	}
}

func TestMIMENULBodyAcquisition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := mimeNULEnv(t)
	raw := []byte("From: sender@example.org\r\nSubject: encoded NUL\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8Ad29ybGQ=\r\n")
	e.Mock.AddMessage("nul", raw, []string{"INBOX"})
	summary := runFullSync(t, e)
	require.Zero(summary.Errors)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"nul"})
	require.NoError(err)
	require.Contains(ids, "nul")
	stored, err := e.Store.GetMessageRaw(ids["nul"])
	require.NoError(err)
	assert.Equal(raw, stored)
	body, err := e.Store.GetMessageBodyText(ids["nul"])
	require.NoError(err)
	assert.Equal("Hello�world", body)
}

func TestMIMENULRetryAfterWriteFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := mimeNULEnv(t)
	source := e.CreateSource(t)
	raw, err := os.ReadFile("../mime/testdata/nul.eml")
	require.NoError(err)
	e.Mock.AddMessage("nul", raw, []string{"INBOX"})
	// Fail after body insertion, at the binary RAW write. Normalizing text must
	// not weaken rollback or consume the retry before the whole snapshot commits.
	allowRaw := acquisitionWriteFault(t, e, "message_raw", "INSERT", "1 = 1")
	summary := runFullSync(t, e)
	require.Equal(int64(1), summary.Errors)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"nul"})
	require.NoError(err)
	assert.Empty(ids)
	var rawCount int
	require.NoError(e.Store.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&rawCount))
	assert.Zero(rawCount)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	assert.Contains(debt[0].ErrorMessage, "synthetic acquisition write failure")
	// Advancing the cursor is safe here because the failure is durably replayable.
	source, err = e.Store.GetSourceByID(source.ID)
	require.NoError(err)
	assert.Equal("1001", source.SyncCursor.String)
	summary = runIncrementalSync(t, e)
	assert.Equal(int64(1), summary.Errors)
	allowRaw()
	summary = runIncrementalSync(t, e)
	require.Zero(summary.Errors)
	ids, err = e.Store.MessageExistsBatch(source.ID, []string{"nul"})
	require.NoError(err)
	require.Contains(ids, "nul")
	stored, err := e.Store.GetMessageRaw(ids["nul"])
	require.NoError(err)
	assert.Equal(raw, stored)
	debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(debt)
}
