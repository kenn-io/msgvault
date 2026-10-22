package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocbankAttachmentProcessingLargeBacklog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	tx, err := f.Store.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	insert, err := tx.PrepareContext(ctx, f.Store.Rebind(`INSERT INTO docbank_attachment_deliveries
 (destination_key, content_hash, upload_name, mime_type, state, node_id, version_id, endpoint, folder)
 VALUES ('dest', ?, '', '', 'delivered', 7, ?, '', '')`))
	require.NoError(err)
	defer func() { assert.NoError(insert.Close()) }()
	for i := range 20000 {
		hash := fmt.Sprintf("file-%05d", i)
		_, err = insert.ExecContext(ctx, hash, hash)
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	for i := range 50 {
		hash := fmt.Sprintf("file-%05d", i)
		d := store.DocbankAttachmentProcessing{ContentHash: hash, VersionID: hash, Fingerprint: "fp", Outcome: "extracted"}
		var next time.Time
		if i < 25 {
			d.Outcome, d.JobID = "requested", "job-"+hash
			next = time.Now().Add(-time.Minute)
		}
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "dest", d, next))
	}
	var cursor store.DocbankAttachmentProcessingCursor
	seen := make(map[string]bool)
	unasked, requested := 0, 0
	for {
		files, err := f.Store.DocbankAttachmentsToProcess(ctx, "dest", "fp", &cursor, 20)
		require.NoError(err)
		if len(files) == 0 {
			break
		}
		for _, d := range files {
			require.False(seen[d.ContentHash], "file visited twice: %s", d.ContentHash)
			seen[d.ContentHash] = true
			if d.Outcome == "requested" {
				assert.Equal(19950, unasked, "unasked files come before due jobs")
				requested++
				require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "dest", d, time.Now().Add(time.Minute)))
			} else {
				unasked++
			}
		}
	}
	assert.Equal(19950, unasked)
	assert.Equal(25, requested)
}

func TestDocbankAttachmentProcessingSelectionCountsAndRetry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	for _, hash := range []string{"a", "b", "c", "d", "e", "f"} {
		require.NoError(f.Store.RecordDocbankAttachment(ctx, "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 7, VersionID: "v-" + hash}, ""))
	}
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "dest", "fp"))
	for _, row := range []struct {
		hash, outcome string
		next          time.Time
	}{
		{"b", "requested", time.Now().Add(-time.Minute)},
		{"c", "extracted", time.Time{}},
		{"d", "provider_timeout", time.Time{}},
		{"e", "operator_required", time.Time{}},
		{"f", "", time.Now().Add(time.Hour)},
	} {
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "dest", store.DocbankAttachmentProcessing{
			ContentHash: row.hash, VersionID: "v-" + row.hash, Fingerprint: "fp", Outcome: row.outcome, JobID: "job-" + row.hash,
		}, row.next))
	}
	due, err := f.Store.DocbankAttachmentsToProcess(ctx, "dest", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 2)
	assert.Equal("a", due[0].ContentHash)
	assert.Equal("b", due[1].ContentHash)
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "dest")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentProcessingSummary{Waiting: 2, Requested: 1, Extracted: 1, Outcomes: map[string]int64{"provider_timeout": 1, "operator_required": 1}}, counts)
	require.NoError(f.Store.RetryDocbankAttachments(ctx, "dest"))
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "dest", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	var hashes []string
	for _, d := range due {
		hashes = append(hashes, d.ContentHash)
	}
	assert.Equal([]string{"a", "d", "e", "b"}, hashes)
	counts, err = f.Store.DocbankAttachmentProcessingCounts(ctx, "dest")
	require.NoError(err)
	assert.Equal(int64(1), counts.Requested)
	assert.Equal(int64(1), counts.Extracted)
	assert.Empty(counts.Outcomes)
	// A new version invalidates the old processing outcome for this hash.
	require.NoError(f.Store.RecordDocbankAttachment(ctx, "dest", store.DocbankAttachmentDelivery{ContentHash: "c"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: "replacement"}, ""))
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "dest", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 5)
	assert.Equal("replacement", due[1].VersionID)
	counts, err = f.Store.DocbankAttachmentProcessingCounts(ctx, "dest")
	require.NoError(err)
	assert.Zero(counts.Extracted)
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "dest", "new-fp"))
	counts, err = f.Store.DocbankAttachmentProcessingCounts(ctx, "dest")
	require.NoError(err)
	assert.Equal(int64(6), counts.Waiting)
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "dest", "new-fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	assert.Len(due, 6)
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "elsewhere", "new-fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	assert.Empty(due)
}

func TestDocbankAttachmentProcessingUpgrade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	_, err := f.Store.DB().Exec(`DROP TABLE docbank_attachment_processing`)
	require.NoError(err)
	version, err := f.Store.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Equal(11, version)
	require.NoError(f.Store.InitSchemaContext(ctx))
	require.NoError(f.Store.ValidateSchemaContext(ctx))
	require.NoError(f.Store.RecordDocbankAttachment(ctx, "dest", store.DocbankAttachmentDelivery{ContentHash: "a"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: "version"}, ""))
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "dest", "fp"))
	require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "dest", store.DocbankAttachmentProcessing{ContentHash: "a", VersionID: "version", Fingerprint: "fp", Outcome: "extracted"}, time.Time{}))
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "dest")
	require.NoError(err)
	assert.Equal(int64(1), counts.Extracted)
}
