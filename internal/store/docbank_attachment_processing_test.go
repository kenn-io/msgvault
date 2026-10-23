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
				assert.Zero(unasked, "due jobs come before unasked files")
				requested++
				require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "dest", d, time.Now().Add(time.Minute)))
			} else {
				require.Equal(25, requested, "poll every due job before planning new files")
				unasked++
			}
		}
	}
	assert.Equal(19950, unasked)
	assert.Equal(25, requested)
}

func TestDocbankAttachmentProcessingSelectionAndCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	for _, hash := range []string{"a", "b", "c", "d", "e", "f"} {
		require.NoError(f.Store.RecordDocbankAttachment(ctx, "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 7, VersionID: "v-" + hash}, ""))
	}
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "dest", "documents", "fp"))
	for _, row := range []struct {
		hash, outcome string
		next          time.Time
	}{
		{"b", "requested", time.Now().Add(-time.Minute)},
		{"c", "extracted", time.Time{}},
		{"d", "provider_timeout", time.Time{}},
		{"e", "operator_required", time.Now().Add(store.BeeperMediaRetryDelay)},
		{"f", "", time.Now().Add(time.Hour)},
	} {
		jobID := "job-" + row.hash
		if row.outcome == "" {
			jobID = ""
		}
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "dest", store.DocbankAttachmentProcessing{
			ContentHash: row.hash, VersionID: "v-" + row.hash, Fingerprint: "fp", Outcome: row.outcome, JobID: jobID,
		}, row.next))
	}
	due, err := f.Store.DocbankAttachmentsToProcess(ctx, "dest", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 2)
	assert.Equal("b", due[0].ContentHash)
	assert.Equal("a", due[1].ContentHash)
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "dest", "documents")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentProcessingSummary{Waiting: 2, Requested: 1, Extracted: 1, Outcomes: map[string]int64{"provider_timeout": 1, "operator_required": 1}}, counts)
	// A new version invalidates the old processing outcome for this hash.
	require.NoError(f.Store.RecordDocbankAttachment(ctx, "dest", store.DocbankAttachmentDelivery{ContentHash: "c"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: "replacement"}, ""))
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "dest", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 3)
	assert.Equal("replacement", due[2].VersionID)
	counts, err = f.Store.DocbankAttachmentProcessingCounts(ctx, "dest", "documents")
	require.NoError(err)
	assert.Zero(counts.Extracted)
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "dest", "documents", "new-fp"))
	counts, err = f.Store.DocbankAttachmentProcessingCounts(ctx, "dest", "documents")
	require.NoError(err)
	assert.Equal(int64(6), counts.Waiting)
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "dest", "new-fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	assert.Len(due, 6)
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "elsewhere", "new-fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	assert.Empty(due)
}

func TestDocbankAttachmentProcessingBackfillPreservesTerminalJobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "dest", "documents", "fp"))
	for _, outcome := range []string{"provider_timeout", "consent", "stale_authority"} {
		require.NoError(f.Store.RecordDocbankAttachment(ctx, "dest", store.DocbankAttachmentDelivery{ContentHash: outcome}, store.DocbankAttachmentRef{NodeID: 7, VersionID: outcome}, ""))
		d := store.DocbankAttachmentProcessing{ContentHash: outcome, VersionID: outcome, Fingerprint: "fp", Outcome: outcome, JobID: "job-" + outcome}
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "dest", d, time.Time{}))
	}
	require.NoError(f.Store.RetryDocbankAttachments(ctx, "dest"))
	due, err := f.Store.DocbankAttachmentsToProcess(ctx, "dest", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 2)
	assert.Equal("consent", due[0].ContentHash)
	assert.Equal("stale_authority", due[1].ContentHash)
	for _, d := range due {
		assert.Empty(d.Outcome)
		assert.Empty(d.JobID)
	}
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "dest", "documents")
	require.NoError(err)
	assert.Equal(int64(2), counts.Waiting)
	assert.Equal(map[string]int64{"provider_timeout": 1}, counts.Outcomes)
	var savedOutcome, jobID string
	require.NoError(f.Store.DB().QueryRow(`SELECT outcome, job_id FROM docbank_attachment_processing WHERE content_hash = 'provider_timeout'`).Scan(&savedOutcome, &jobID))
	assert.Equal("provider_timeout", savedOutcome)
	assert.Equal("job-provider_timeout", jobID)
}
