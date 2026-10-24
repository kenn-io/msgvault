package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentSearchVersionsAndOccurrences(t *testing.T) {
	t.Run("versions and occurrences", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := storetest.New(t)
		participant := f.EnsureParticipant("reader@example.com", "Reader", "example.com")
		versionIDs := map[string]string{
			"old":                   "00000000-0000-4000-8000-000000000001",
			"new":                   "00000000-0000-4000-8000-000000000004",
			"undated":               "00000000-0000-4000-8000-000000000002",
			"deleted":               "00000000-0000-4000-8000-000000000005",
			"hidden":                "00000000-0000-4000-8000-000000000006",
			"other person":          "00000000-0000-4000-8000-000000000007",
			"storage path fallback": "00000000-0000-4000-8000-000000000003",
			"dated tie":             "00000000-0000-4000-8000-000000000008",
			"undated tie":           "00000000-0000-4000-8000-000000000009",
			"new earlier":           "00000000-0000-4000-8000-000000000004",
		}
		var visible []int64
		for i, name := range []string{"old", "new", "undated", "deleted", "hidden", "other person", "storage path fallback", "dated tie", "undated tie", "new earlier"} {
			mid := f.CreateMessage(name)
			hash := fmt.Sprintf("%064x", i+1)
			if name == "new earlier" {
				hash = fmt.Sprintf("%064x", 2)
			}
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: name + ".pdf", StoragePath: hash[:2] + "/" + hash, State: "stored"}))
			var date any
			if name != "undated" && name != "undated tie" {
				date = time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC)
			}
			if name == "dated tie" {
				date = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			if name == "new earlier" {
				date = time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
			}
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=?,received_at=NULL,internal_date=NULL,sender_id=? WHERE id=?`), date, participant, mid)
			require.NoError(err)
			switch name {
			case "deleted":
				_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), time.Now().UTC(), mid)
			case "hidden":
				_, err = f.Store.MergeDuplicates(f.CreateMessage("survivor"), []int64{mid}, "hidden")
			case "other person":
				_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sender_id=NULL WHERE id=?`), mid)
			case "storage path fallback":
				_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET content_hash=NULL,source_attachment_id='slack:file' WHERE message_id=?`), mid)
			}
			require.NoError(err)
			require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash, MIMEType: "application/pdf"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: versionIDs[name]}, ""))
			if name != "deleted" && name != "hidden" && name != "other person" {
				visible = append(visible, mid)
			}
		}
		scope := &personscope.Scope{ParticipantIDs: []int64{participant}, Directions: []personscope.Direction{personscope.FromPerson}}
		ids, scopeExceeded, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", scope)
		require.NoError(err)
		assert.False(scopeExceeded)
		assert.ElementsMatch([]string{"00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004", "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000008", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000009"}, ids)
		occurrences, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, ids, 100)
		require.NoError(err)
		var mids []int64
		for _, o := range occurrences {
			mids = append(mids, o.MessageID)
		}
		assert.Equal([]int64{visible[3], visible[1], visible[0], visible[4], visible[6], visible[2], visible[5]}, mids)
		other, err := f.Store.DocumentSearchOccurrences(t.Context(), "other", scope, ids, 100)
		require.NoError(err)
		assert.Empty(other)
		onlyNew, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, []string{"00000000-0000-4000-8000-000000000004"}, 100)
		require.NoError(err)
		require.Len(onlyNew, 2)
		assert.Equal("new.pdf", onlyNew[0].Filename)
		assert.Equal("new earlier.pdf", onlyNew[1].Filename)
	})
	t.Run("per-version limit and duplicate collapse", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := storetest.New(t)
		versions := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}
		want := make(map[string][]int64)
		for i, version := range versions {
			hash := fmt.Sprintf("%064x", i+1)
			require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash, MIMEType: "application/pdf"}, store.DocbankAttachmentRef{NodeID: int64(i + 1), VersionID: version}, ""))
			for day := 5; day >= 1; day-- {
				mid := f.CreateMessage(fmt.Sprintf("version-%d-day-%d", i, day))
				require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "original", ContentHash: hash, Filename: "report.pdf", StoragePath: hash[:2] + "/" + hash, State: "stored"}))
				// The same file can appear twice under separate source attachment IDs.
				require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "duplicate", SourceAttachmentID: "duplicate", ContentHash: hash, Filename: "copy.pdf", StoragePath: hash[:2] + "/" + hash, State: "stored"}))
				_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC), mid)
				require.NoError(err)
				if day >= 3 {
					want[version] = append(want[version], mid)
				}
			}
		}
		var attachments int
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachments))
		require.Equal(20, attachments)
		occurrences, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", nil, versions, 2)
		require.NoError(err)
		got := make(map[string][]int64)
		for _, occurrence := range occurrences {
			got[occurrence.VersionID] = append(got[occurrence.VersionID], occurrence.MessageID)
		}
		assert.Equal(want, got)
	})
}

func TestDocumentSearchVersionsTextFormats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	mid := f.CreateMessage("text-formats")
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	formats := []struct {
		mime    string
		version string
	}{
		{"application/pdf", "pdf"},
		{"text/csv; charset=utf-8", "csv"},
		{"application/json", "json"},
		{"message/rfc822", "email"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "word"},
		{"IMAGE/PNG", "ocr-image"},
		{"application/PDF; charset=binary", "parameterized"},
		{"image/tiff", "supplied-text"},
		{"audio/mpeg", ""},
		{"image/gif", ""},
		{"application/octet-stream", ""},
	}
	want := []string{"pdf", "csv", "json", "email", "word", "ocr-image", "parameterized", "supplied-text"}
	for i := range 4110 {
		mimeType, version := "application/zip", fmt.Sprintf("archive-%d", i)
		if i < len(formats) {
			mimeType = formats[i].mime
			if formats[i].version != "" {
				version = formats[i].version
			}
		}
		hash := fmt.Sprintf("%064x", 0xabcdef+i)
		_, err := tx.Exec(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,content_hash,storage_path,attachment_state) VALUES (?,'file',?,?,'stored')`), mid, strings.ToUpper(hash), hash[:2]+"/"+hash)
		require.NoError(err)
		_, err = tx.Exec(f.Store.Rebind(`INSERT INTO docbank_attachment_deliveries(destination_key,content_hash,upload_name,mime_type,state,node_id,version_id,endpoint,folder) VALUES ('attachments',?,'file',?,'delivered',7,?,'','')`), hash, mimeType, version)
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	ids, exceeded, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", nil)
	require.NoError(err)
	assert.False(exceeded)
	assert.ElementsMatch(want, ids)
}
