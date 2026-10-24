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
	documents := make([]storetest.DocumentAttachment, 20000)
	for i := range documents {
		documents[i] = storetest.DocumentAttachment{MIMEType: "application/pdf", VersionID: fmt.Sprintf("%064x", 0xabcdef+i)}
	}
	f.SeedDocumentAttachments(f.CreateMessage("backlog"), documents)
	for i := range 50 {
		hash := fmt.Sprintf("%064x", 0xabcdef+i)
		d := store.DocbankAttachmentProcessing{ContentHash: hash, VersionID: hash, Fingerprint: "fp", Outcome: "extracted"}
		var next time.Time
		if i < 25 {
			d.Outcome, d.JobID = "requested", "job-"+hash
			next = time.Now().Add(-time.Minute)
		}
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", d, next))
	}
	var cursor store.DocbankAttachmentProcessingCursor
	seen := make(map[string]bool)
	unasked, requested := 0, 0
	for {
		files, err := f.Store.DocbankAttachmentsToProcess(ctx, "attachments", "fp", &cursor, 20)
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
				d.Outcome = "extracted"
				require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", d, time.Time{}))
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
	hashes := make(map[string]string)
	documents := make([]storetest.DocumentAttachment, 7)
	for i, letter := range []string{"a", "b", "c", "d", "e", "f", "image"} {
		hashes[letter] = fmt.Sprintf("%064x", 0xabcdef+i)
		documents[i] = storetest.DocumentAttachment{MIMEType: "application/pdf", VersionID: "v-" + letter}
	}
	documents[6].MIMEType = "image/png"
	f.SeedDocumentAttachments(f.CreateMessage("visible"), documents)
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "attachments", "documents", "fp"))
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
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", store.DocbankAttachmentProcessing{
			ContentHash: hashes[row.hash], VersionID: "v-" + row.hash, Fingerprint: "fp", Outcome: row.outcome, JobID: jobID,
		}, row.next))
	}
	due, err := f.Store.DocbankAttachmentsToProcess(ctx, "attachments", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 2)
	assert.Equal(hashes["b"], due[0].ContentHash)
	assert.Equal(hashes["a"], due[1].ContentHash)
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "attachments", "documents")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentProcessingSummary{Waiting: 2, Requested: 1, Extracted: 1, Outcomes: map[string]int64{"provider_timeout": 1, "operator_required": 1}}, counts)
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "attachments", "documents", "new-fp"))
	counts, err = f.Store.DocbankAttachmentProcessingCounts(ctx, "attachments", "documents")
	require.NoError(err)
	assert.Equal(int64(6), counts.Waiting)
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "attachments", "new-fp", new(store.DocbankAttachmentProcessingCursor), 20)
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
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "attachments", "documents", "fp"))
	documents := []storetest.DocumentAttachment{{MIMEType: "application/pdf", VersionID: "provider_timeout"}, {MIMEType: "application/pdf", VersionID: "consent"}, {MIMEType: "application/pdf", VersionID: "stale_authority"}, {MIMEType: "application/pdf", VersionID: "pdf_password_required"}, {MIMEType: "application/pdf", VersionID: "not_found"}}
	f.SeedDocumentAttachments(f.CreateMessage("backfill"), documents)
	for i, outcome := range []string{"provider_timeout", "consent", "stale_authority", "pdf_password_required", "not_found"} {
		d := store.DocbankAttachmentProcessing{ContentHash: fmt.Sprintf("%064x", 0xabcdef+i), VersionID: outcome, Fingerprint: "fp", Outcome: outcome, JobID: "job-" + outcome}
		if outcome == "not_found" {
			d.JobID = ""
		}
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", d, time.Time{}))
	}
	require.NoError(f.Store.RetryDocbankAttachments(ctx, "attachments"))
	due, err := f.Store.DocbankAttachmentsToProcess(ctx, "attachments", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 4)
	assert.Equal(fmt.Sprintf("%064x", 0xabcdf0), due[0].ContentHash)
	assert.Equal(fmt.Sprintf("%064x", 0xabcdf1), due[1].ContentHash)
	assert.Equal(fmt.Sprintf("%064x", 0xabcdf2), due[2].ContentHash)
	assert.Equal(fmt.Sprintf("%064x", 0xabcdf3), due[3].ContentHash)
	for _, d := range due {
		assert.Empty(d.Outcome)
		assert.Empty(d.JobID)
	}
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "attachments", "documents")
	require.NoError(err)
	assert.Equal(int64(4), counts.Waiting)
	assert.Equal(map[string]int64{"provider_timeout": 1}, counts.Outcomes)
	var savedOutcome, jobID string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT outcome, job_id FROM docbank_attachment_processing WHERE content_hash = ?`), fmt.Sprintf("%064x", 0xabcdef)).Scan(&savedOutcome, &jobID))
	assert.Equal("provider_timeout", savedOutcome)
	assert.Equal("job-provider_timeout", jobID)
}
