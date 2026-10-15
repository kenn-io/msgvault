package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocbankAttachmentStatusCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("a", 64)
	mid := f.CreateMessage("stored")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: "report.pdf", StoragePath: "aa/" + hash, State: "stored"}))
	for i := range 9 {
		mid := f.CreateMessage(fmt.Sprintf("shared-%d", i))
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: strings.ToUpper(hash), Filename: "report.pdf", StoragePath: "aa/" + hash, State: "stored"}))
	}

	var checkpoints int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key='beeper_media_scan:dest'`).Scan(&checkpoints))
	require.Zero(checkpoints)
	status, err := f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentSummary{Remaining: 1, Reasons: map[string]int64{}}, status)
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key='beeper_media_scan:dest'`).Scan(&checkpoints))
	assert.Zero(checkpoints, "status must leave an absent checkpoint in memory")
	require.NoError(f.Store.QueueDocbankAttachmentIntents(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}))
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentSummary{Sending: 1, Reasons: map[string]int64{}}, status)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, "source_unavailable"))
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentSummary{Failed: 1, Reasons: map[string]int64{"source_unavailable": 1}}, status)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, ""))
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Zero(status.Remaining, "delivered files stay excluded before discovery and during rescans")
	other, err := f.Store.DocbankAttachmentStatus(t.Context(), "other")
	require.NoError(err)
	assert.Equal(int64(1), other.Remaining, "receipts belong to their destination")
}

func TestDocbankAttachmentDiscoveryBoundsConcurrentImports(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	mid := f.CreateMessage("continuous-import")
	add := func(start int) {
		for i := start; i < start+501; i++ {
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "metadata", SourcePartKey: fmt.Sprintf("part:%d", i)}))
		}
	}
	add(0)
	hash := strings.Repeat("a", 64)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET content_hash=?,storage_path=?,attachment_state='stored' WHERE source_part_key='part:500'`), strings.ToUpper(hash), "aa/"+hash)
	require.NoError(err)
	page, err := f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(int64(501), page.After.PassHighWater)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	add(501)
	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(int64(501), page.After.AfterAttachmentID)
	require.Len(page.Files, 1)
	assert.Equal(hash, page.Files[0].ContentHash)
	assert.Equal(int64(501), page.After.PassHighWater)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	add(1002)
	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.False(page.More)
	assert.Zero(page.After.PassHighWater)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(err)
	assert.True(consumer.ReconciliationComplete)
}

func TestDocbankAttachmentPagesAndBackfill(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("a", 64)
	mid := f.CreateMessage("stored")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: "report.pdf", MIMEType: "application/pdf", Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	page, err := f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	require.Len(page.Files, 1)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", page.Files[0], store.DocbankAttachmentRef{}, "timeout"))
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	due, err := f.Store.SendingDocbankAttachments(t.Context(), "dest")
	require.NoError(err)
	assert.Empty(due)
	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.False(page.More)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	assert.True(page.After.NextFullScanAt.After(time.Now()))
	before := page.After
	after := before
	after.NextFullScanAt = time.Now().Add(-time.Minute)
	swapped, err := f.Store.AdvanceDocbankAttachmentScan(t.Context(), "dest", before, after)
	require.NoError(err)
	require.True(swapped)
	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.True(page.More)
	assert.Zero(page.After.AfterAttachmentID)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	require.NoError(f.Store.RetryDocbankAttachments(t.Context(), "dest"))
	reset, err := f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
	require.NoError(err)
	assert.NotEmpty(reset.Generation)
	assert.NotEqual(page.After.Generation, reset.Generation)
	assert.Equal(store.BeeperMediaScan{DestinationKey: "dest", Generation: reset.Generation}, reset)
	swapped, err = f.Store.AdvanceDocbankAttachmentScan(t.Context(), "dest", page.After, before)
	require.NoError(err)
	assert.False(swapped, "backfill rejects a stale page after reloading its checkpoint")
	due, err = f.Store.SendingDocbankAttachments(t.Context(), "dest")
	require.NoError(err)
	require.Len(due, 1)
	assert.Equal("report.pdf", due[0].Name)
	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.Empty(page.Files)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.Empty(page.Files, "outcomes are skipped during discovery")
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", due[0], store.DocbankAttachmentRef{Endpoint: "https://docbank.example.com", Folder: "/msgvault", NodeID: 8, VersionID: "v1"}, ""))
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	status, err := f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentSummary{Delivered: 1, Reasons: map[string]int64{}}, status)
	before, err = f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
	require.NoError(err)
	_, err = f.Store.DB().Exec(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP`)
	require.NoError(err)
	_, err = f.Store.MergeDuplicates(f.CreateMessage("survivor"), []int64{mid}, "hidden")
	require.NoError(err)
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "other-consumer")
	require.NoError(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(t.Context(), consumer.ConsumerKey, consumer.BaselineSequence))
	restored, err := f.Store.UndoDedup("hidden")
	require.NoError(err)
	assert.Equal(int64(1), restored)
	after, err = f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(before, after, "undoing dedup preserves discovery progress")
	changes, err := f.Store.ListAttachmentChanges(t.Context(), "other-consumer", 500)
	require.NoError(err)
	assert.Empty(changes, "source-deleted restores preserve the journal's live definition")
}

func TestDocbankAttachmentReceiptsBeyondChunk(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(err)
	var hashes []string
	for i := range 513 {
		hash := fmt.Sprintf("%064x", i+1)
		hashes = append(hashes, hash)
		require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{Endpoint: "https://docbank.example.com", Folder: "/msgvault", NodeID: int64(i + 1), VersionID: "v1"}, ""))
	}
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "other", store.DocbankAttachmentDelivery{ContentHash: hashes[512]}, store.DocbankAttachmentRef{Folder: "/old", NodeID: 9}, ""))
	prefix := "main."
	if f.Store.IsPostgreSQL() {
		var schema string
		require.NoError(f.Store.DB().QueryRow(`SELECT current_schema()`).Scan(&schema))
		prefix = schema + "."
	}
	uppercaseHash := hashes[10]
	hashes[10] = strings.ToUpper(uppercaseHash)
	hashes = append(hashes, hashes[0], "")
	refs, err := store.LoadDocbankAttachmentRefs(t.Context(), f.Store.DB(), f.Store.Rebind, prefix, hashes)
	require.NoError(err)
	assert.Len(refs, 513)
	require.Len(refs[hashes[512]], 1)
	assert.Equal(int64(513), refs[hashes[512]][0].NodeID)
	require.Len(refs[uppercaseHash], 1)
	assert.Equal(int64(11), refs[uppercaseHash][0].NodeID)
	require.Len(refs[hashes[0]], 1)
	assert.Equal(int64(1), refs[hashes[0]][0].NodeID)
}

func TestDocbankAttachmentDiscoveryEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		eligible     bool
	}{
		{"stored", `attachment_state='stored'`, true},
		{"legacy stored", `attachment_state=''`, true},
		{"unknown size", `size=NULL`, true},
		{"empty file", `size=0`, true},
		{"hidden message", `message_id=message_id`, true},
		{"source deleted message", `message_id=message_id`, true},
		{"unstored", `attachment_state='pending'`, false},
		{"URL", `storage_path='https://example.com/file'`, false},
		{"missing path", `storage_path=''`, false},
		{"unsupported alias", `content_hash='',source_attachment_id='gmail:alias'`, false},
		{"alias only", `content_hash='',source_attachment_id='slack:alias'`, true},
		{"empty alias", `content_hash='',size=0,source_attachment_id='slack:alias'`, true},
		{"invalid alias path", `content_hash='',storage_path='legacy/file',source_attachment_id='slack:alias'`, false},
		{"Discord alias", `content_hash='',source_attachment_id='discord:alias'`, true},
		{"uppercase Slack alias", `content_hash='',storage_path='AA/` + strings.Repeat("A", 64) + `',source_attachment_id='slack:alias'`, false},
		{"uppercase Discord alias", `content_hash='',storage_path='AA/` + strings.Repeat("A", 64) + `',source_attachment_id='discord:alias'`, false},
		{"non-hex Slack alias", `content_hash='',storage_path='aa/` + strings.Repeat("a", 63) + `g',source_attachment_id='slack:alias'`, false},
		{"non-hex Discord alias", `content_hash='',storage_path='aa/` + strings.Repeat("a", 63) + `g',source_attachment_id='discord:alias'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			hash := strings.Repeat("a", 64)
			mid := f.CreateMessage("discovery")
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: "file.pdf", MIMEType: "application/pdf", StoragePath: "aa/" + hash, Size: 10, State: "stored"}))
			_, err := f.Store.DB().Exec(`UPDATE attachments SET ` + tc.change)
			require.NoError(err)
			if tc.name == "hidden message" {
				_, err = f.Store.DB().Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP`)
				require.NoError(err)
			}
			if tc.name == "source deleted message" {
				_, err = f.Store.DB().Exec(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP`)
				require.NoError(err)
			}
			page, err := f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
			require.NoError(err)
			assert.Len(page.Files, map[bool]int{true: 1}[tc.eligible])
			status, err := f.Store.DocbankAttachmentStatus(t.Context(), "dest")
			require.NoError(err)
			assert.Equal(map[bool]int64{true: 1}[tc.eligible], status.Remaining)
			require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
			status, err = f.Store.DocbankAttachmentStatus(t.Context(), "dest")
			require.NoError(err)
			assert.Equal(map[bool]int64{true: 1}[tc.eligible], status.Remaining, "discovery alone leaves eligible files undelivered")
		})
	}
}

func TestDocbankAttachmentReceiptAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(err)
	hash := strings.Repeat("a", 64)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 8, VersionID: "v1"}, ""))
	for _, tc := range []struct {
		name, contentHash, path, provider string
		refs                              int
	}{
		{"stored", hash, "aa/" + hash, "", 1},
		{"Slack alias", "", "aa/" + hash, "slack:synthetic", 1},
		{"Discord alias", "", "aa/" + hash, "discord:synthetic", 1},
		{"Beeper pending", "", "aa/" + hash, "beeper:synthetic", 0},
		{"other hashless", "", "aa/" + hash, "gmail:synthetic", 0},
		{"untrusted path", "", "legacy/file", "slack:synthetic", 0},
		{"URL", hash, "https://example.com/file", "slack:synthetic", 0},
		{"uppercase URL", hash, "HTTPS://example.com/file", "slack:synthetic", 0},
	} {
		mid := f.CreateMessage(tc.name)
		assert.Equal(map[int]string{1: hash}[tc.refs], store.AttachmentReceiptHash(tc.contentHash, tc.path, tc.provider), tc.name)
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: tc.contentHash, StoragePath: tc.path, SourceAttachmentID: tc.provider, State: "stored"}))
		msg, err := f.Store.GetMessageContext(t.Context(), mid)
		require.NoError(err)
		require.Len(msg.Attachments, 1)
		att := msg.Attachments[0]
		assert.Len(att.Docbank, tc.refs, tc.name)
		wantHash := tc.contentHash
		if tc.refs == 1 {
			wantHash = hash
		} else if tc.name == "URL" || tc.name == "uppercase URL" {
			wantHash = ""
		}
		assert.Equal(wantHash, att.ContentHash, tc.name)
		if tc.name == "URL" || tc.name == "uppercase URL" {
			assert.Equal(tc.path, att.URL, tc.name)
		}
		file, err := f.Store.GetFileMetadata(t.Context(), att.ID)
		require.NoError(err)
		require.NotNil(file)
		assert.Len(file.Docbank, tc.refs, tc.name)
		if tc.path == "aa/"+hash {
			assert.Equal(hash, file.ContentHash, tc.name)
		} else {
			assert.Empty(file.ContentHash, tc.name)
		}
		window, err := f.Store.GetConversationWindow(f.ConvID, mid, 0, 0)
		require.NoError(err)
		require.Len(window.Messages, 1)
		require.Len(window.Messages[0].Attachments, 1)
		assert.Len(window.Messages[0].Attachments[0].Docbank, tc.refs, tc.name)
		wantHash = tc.contentHash
		if tc.name == "URL" || tc.name == "uppercase URL" {
			wantHash = ""
			assert.Equal(tc.path, window.Messages[0].Attachments[0].URL, tc.name)
		}
		assert.Equal(wantHash, window.Messages[0].Attachments[0].ContentHash, tc.name)
	}
}

func TestDocbankAttachmentReregistrationRestartsAfterCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	mid := f.CreateMessage("stored while mirroring is off")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "part:1", Filename: "file.pdf", State: "pending"}))
	page, err := f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	require.Positive(page.After.AfterAttachmentID)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	require.False(page.More)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "dest", page))
	baseline := page.After.BaselineSequence
	require.NoError(f.Store.RemoveInactiveDocbankAttachmentConsumers(t.Context(), "other"))
	hash := strings.Repeat("a", 64)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "part:1", Filename: "file.pdf", ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))

	// Cancel after registration commits, before discovery can reset the retained scan.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	consumer, created, err := f.Store.RegisterAttachmentChangeConsumer(ctx, store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(err)
	require.True(created)
	require.Equal(baseline, consumer.BaselineSequence)
	require.False(consumer.ReconciliationComplete)
	cancel()
	_, err = f.Store.NextDocbankAttachmentPage(ctx, "dest")
	require.ErrorIs(err, context.Canceled)

	page, err = f.Store.NextDocbankAttachmentPage(t.Context(), "dest")
	require.NoError(err)
	assert.Zero(page.Before.AfterAttachmentID)
	require.Len(page.Files, 1)
	assert.Equal(hash, page.Files[0].ContentHash)
	status, err := f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(int64(1), status.Remaining)
}

func TestDocbankAttachmentConsumerRemovalLocksOnlyRetiredConsumers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	page, err := f.Store.NextDocbankAttachmentPage(t.Context(), "retired")
	require.NoError(err)
	require.NoError(f.Store.FinishDocbankAttachmentPage(t.Context(), "retired", page))
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "other-consumer")
	require.NoError(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(t.Context(), consumer.ConsumerKey, consumer.BaselineSequence))
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage("later"), store.AttachmentWrite{Filename: "later.pdf"}))
	changes, err := f.Store.ListAttachmentChanges(t.Context(), consumer.ConsumerKey, 500)
	require.NoError(err)
	require.NotEmpty(changes)
	require.NoError(f.Store.AdvanceAttachmentChangeConsumer(t.Context(), consumer.ConsumerKey, changes[len(changes)-1].Sequence))
	require.NoError(f.Store.RemoveInactiveDocbankAttachmentConsumers(t.Context(), "dest"))
	var retained int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachment_change_log`).Scan(&retained))
	assert.Zero(retained)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL table locks")
	}
	_, _, err = f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(err)
	_, _, err = f.Store.RegisterAttachmentChangeConsumer(t.Context(), "other-consumer")
	require.NoError(err)
	tx, err := f.Store.DB().Begin()
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`LOCK TABLE attachments IN ROW EXCLUSIVE MODE`)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(f.Store.RemoveInactiveDocbankAttachmentConsumers(ctx, "dest"))
	require.NoError(tx.Rollback())
	_, _, err = f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("retired"))
	require.NoError(err)
	tx, err = f.Store.DB().Begin()
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`LOCK TABLE attachments IN ROW EXCLUSIVE MODE`)
	require.NoError(err)
	ctx, cancelLock := context.WithTimeout(t.Context(), time.Second)
	defer cancelLock()
	require.Error(f.Store.RemoveInactiveDocbankAttachmentConsumers(ctx, "dest"))
	require.NoError(tx.Rollback())
	require.NoError(f.Store.RemoveInactiveDocbankAttachmentConsumers(t.Context(), "dest"))
	_, err = f.Store.GetAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("retired"))
	require.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing)
	_, err = f.Store.GetAttachmentChangeConsumer(t.Context(), "other-consumer")
	assert.NoError(err)
}

func TestDocbankAttachmentRetryPagesAtScale(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	mid := f.CreateMessage("retry queue")
	hashSQL := "printf('%064x',i)"
	if f.Store.IsPostgreSQL() {
		hashSQL = "lpad(to_hex(i),64,'0')"
	}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`WITH RECURSIVE ids(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM ids WHERE i<5000) INSERT INTO attachments(message_id,content_hash,storage_path,attachment_state) SELECT ?,`+hashSQL+`,SUBSTR(`+hashSQL+`,1,2)||'/'||`+hashSQL+`,'stored' FROM ids`), mid)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`INSERT INTO docbank_attachment_deliveries(destination_key,content_hash,upload_name,mime_type,state,endpoint,folder,attempts) SELECT 'dest',content_hash,'file.pdf','application/pdf','sending','','',1 FROM attachments ORDER BY id LIMIT 5000`)
	require.NoError(err)
	seen := make(map[string]bool)
	for range 10 {
		page, err := f.Store.SendingDocbankAttachments(t.Context(), "dest")
		require.NoError(err)
		require.Len(page, 500)
		var args []any
		for _, d := range page {
			assert.False(seen[d.ContentHash], "retry pages must advance")
			seen[d.ContentHash] = true
			args = append(args, d.ContentHash)
		}
		_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE docbank_attachment_deliveries SET attempts=attempts+1 WHERE content_hash IN (?`+strings.Repeat(",?", len(args)-1)+`)`), args...)
		require.NoError(err)
	}
	assert.Len(seen, 5000)
}

func TestDocbankAttachmentDeletedAfterIntent(t *testing.T) {
	for _, beforeIntent := range []bool{false, true} {
		t.Run(fmt.Sprintf("before_intent=%t", beforeIntent), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			hash := strings.Repeat("a", 64)
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage("deleted"), store.AttachmentWrite{ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
			if beforeIntent {
				_, err := f.Store.DB().Exec(`DELETE FROM attachments`)
				require.NoError(err)
				_, err = f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash})
				require.ErrorIs(err, sql.ErrNoRows)
				status, err := f.Store.DocbankAttachmentStatus(t.Context(), "dest")
				require.NoError(err)
				assert.Equal(store.DocbankAttachmentSummary{Reasons: map[string]int64{}}, status)
				return
			}
			_, err := f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash})
			require.NoError(err)
			_, err = f.Store.DB().Exec(`DELETE FROM attachments`)
			require.NoError(err)
			status, err := f.Store.DocbankAttachmentStatus(t.Context(), "dest")
			require.NoError(err)
			assert.Zero(status.Sending)
			page, err := f.Store.SendingDocbankAttachments(t.Context(), "dest")
			require.NoError(err)
			assert.Empty(page)
			status, err = f.Store.DocbankAttachmentStatus(t.Context(), "dest")
			require.NoError(err)
			assert.Equal(int64(1), status.Failed)
			assert.Equal(int64(1), status.Reasons["source_unavailable"])
		})
	}
}

func TestDocbankAttachmentRetryDelay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("a", 64)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage("delayed retry"), store.AttachmentWrite{ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
	d := store.DocbankAttachmentDelivery{ContentHash: hash, Name: "original.pdf"}
	require.NoError(f.Store.QueueDocbankAttachmentIntents(t.Context(), "dest", d))
	status, err := f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Empty(status.Reasons)
	d, err = f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", d)
	require.NoError(err)
	assert.Equal(1, d.Attempts)
	require.NoError(f.Store.QueueDocbankAttachmentIntents(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash, Name: "renamed.pdf"}))
	intents, err := f.Store.SendingDocbankAttachments(t.Context(), "dest")
	require.NoError(err)
	require.Len(intents, 1)
	assert.Equal("original.pdf", intents[0].Name)
	assert.Equal(1, intents[0].Attempts)
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(int64(1), status.Reasons["retrying"])
	before := time.Now().UTC().Truncate(time.Second).Add(5 * time.Minute)
	require.NoError(f.Store.RecordDocbankAttachmentRetry(t.Context(), "dest", d, "forbidden"))
	var retryAt time.Time
	require.NoError(f.Store.DB().QueryRow(`SELECT next_action_at FROM docbank_attachment_deliveries`).Scan(&retryAt))
	assert.False(retryAt.Before(before))
	intents, err = f.Store.SendingDocbankAttachments(t.Context(), "dest")
	require.NoError(err)
	assert.Empty(intents)
	_, err = f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", d)
	require.ErrorIs(err, sql.ErrNoRows)
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "dest")
	require.NoError(err)
	assert.Equal(int64(1), status.Sending)
	assert.Equal(int64(1), status.Reasons["forbidden"])
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE docbank_attachment_deliveries SET next_action_at=?`), time.Now().UTC().Add(-time.Minute))
	require.NoError(err)
	intents, err = f.Store.SendingDocbankAttachments(t.Context(), "dest")
	require.NoError(err)
	require.Len(intents, 1)
	d, err = f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash, Name: "renamed.pdf"})
	require.NoError(err)
	assert.Equal(2, d.Attempts)
	assert.Equal("original.pdf", d.Name)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", d, store.DocbankAttachmentRef{}, "source_unavailable"))
	d, err = f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", d)
	require.NoError(err)
	assert.Equal(1, d.Attempts)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", d, store.DocbankAttachmentRef{}, "source_unavailable"))
	require.NoError(f.Store.QueueDocbankAttachmentIntents(t.Context(), "dest", d))
	intents, err = f.Store.SendingDocbankAttachments(t.Context(), "dest")
	require.NoError(err)
	require.Len(intents, 1)
	assert.Zero(intents[0].Attempts)
	d, err = f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", d)
	require.NoError(err)
	assert.Equal(1, d.Attempts)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", d, store.DocbankAttachmentRef{NodeID: 7}, ""))
	_, err = f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", d)
	require.ErrorIs(err, sql.ErrNoRows)
}

func TestDocbankAttachmentReceiptsFollowActiveDestination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	key := store.DocbankAttachmentConsumerKey("dest")
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), key)
	require.NoError(err)
	hash := strings.Repeat("a", 64)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 8, VersionID: "v1"}, ""))
	receipts := func() []store.DocbankAttachmentRef {
		refs, err := store.LoadDocbankAttachmentRefs(t.Context(), f.Store.DB(), f.Store.Rebind, "", []string{hash})
		require.NoError(err)
		return refs[hash]
	}
	require.Len(receipts(), 1)
	require.NoError(f.Store.RemoveInactiveDocbankAttachmentConsumers(t.Context(), ""))
	assert.Empty(receipts(), "a disabled mirror hides its receipts")
	_, _, err = f.Store.RegisterAttachmentChangeConsumer(t.Context(), key)
	require.NoError(err)
	assert.Len(receipts(), 1, "re-enabling the destination shows the kept delivery record")
}
