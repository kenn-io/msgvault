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
	documents := make([]storetest.DocumentAttachment, 1000)
	for i := range documents {
		documents[i] = storetest.DocumentAttachment{MIMEType: "application/pdf", VersionID: fmt.Sprintf("%064x", 0xabcdef+i)}
	}
	f.SeedDocumentAttachments(f.CreateMessage("backlog"), documents)
	for i := range 50 {
		hash := fmt.Sprintf("%064x", 0xabcdef+i)
		d := store.DocbankAttachmentProcessing{ContentHash: hash, VersionID: hash, Fingerprint: "fp", Outcome: "extracted", JobID: hash}
		var next time.Time
		if i < 25 {
			d.Outcome = "requested"
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
		if len(seen) == 0 {
			require.Len(files, 20)
			for _, d := range files[:5] {
				require.Empty(d.Outcome, "unasked files run before slow polls can exhaust the pass budget")
			}
			assert.Equal("requested", files[5].Outcome)
		}
		batchUnasked := 0
		for i, d := range files {
			require.False(seen[d.ContentHash], "file visited twice: %s", d.ContentHash)
			seen[d.ContentHash] = true
			if d.Outcome == "requested" {
				requested++
				d.Outcome = "extracted"
				require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", d, time.Time{}))
			} else {
				assert.Equal(batchUnasked, i, "unasked files come first")
				batchUnasked++
				unasked++
			}
		}
		assert.LessOrEqual(batchUnasked, 5, "discovery leaves room for due polls")
	}
	assert.Equal(950, unasked)
	assert.Equal(25, requested)
	for i := range 25 {
		hash := fmt.Sprintf("%064x", 0xabcdef+i)
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", store.DocbankAttachmentProcessing{
			ContentHash: hash, Fingerprint: "fp", Outcome: "requested", JobID: hash,
		}, time.Now().Add(-time.Minute)))
	}
	files, err := f.Store.DocbankAttachmentsToProcess(ctx, "attachments", "fp", &cursor, 20)
	require.NoError(err)
	assert.Len(files, 20, "polls use the whole batch after unasked files are exhausted")
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
	for i, row := range []struct {
		hash, outcome string
		next          time.Time
	}{
		{"b", "requested", time.Now().Add(-time.Minute)},
		{"c", "extracted", time.Time{}},
		{"d", "provider_timeout", time.Time{}},
		{"e", "operator_required", time.Time{}},
		{"f", "", time.Now().Add(time.Hour)},
	} {
		jobID := fmt.Sprintf("%064x", i+1)
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
	assert.Equal(hashes["a"], due[0].ContentHash)
	assert.Equal(hashes["b"], due[1].ContentHash)
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "attachments", "documents")
	require.NoError(err)
	assert.Equal(store.DocbankAttachmentProcessingSummary{Waiting: 2, Requested: 1, Extracted: 1, Outcomes: map[string]int64{"provider_timeout": 1, "operator_required": 1}}, counts)
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "attachments", "documents", "new-fp"))
	counts, err = f.Store.DocbankAttachmentProcessingCounts(ctx, "attachments", "documents")
	require.NoError(err)
	assert.Equal(int64(6), counts.Waiting)
	var cursor store.DocbankAttachmentProcessingCursor
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "attachments", "new-fp", &cursor, 20)
	require.NoError(err)
	assert.Len(due, 5)
	due, err = f.Store.DocbankAttachmentsToProcess(ctx, "elsewhere", "new-fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	assert.Empty(due)
}

func TestDocbankAttachmentProcessingBackfillRetriesRecoverableOutcomes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	require.NoError(f.Store.SaveDocbankAttachmentProcessingFingerprint(ctx, "attachments", "documents", "fp"))
	documents := []storetest.DocumentAttachment{{MIMEType: "application/pdf", VersionID: "provider_timeout"}, {MIMEType: "application/pdf", VersionID: "consent"}, {MIMEType: "application/pdf", VersionID: "stale_authority"}, {MIMEType: "application/pdf", VersionID: "pdf_password_required"}, {MIMEType: "application/pdf", VersionID: "not_found"}, {MIMEType: "application/pdf", VersionID: "rendition_operator_required"}, {MIMEType: "application/pdf", VersionID: "operator_required"}, {MIMEType: "application/pdf", VersionID: "pending"}}
	documents = append(documents, storetest.DocumentAttachment{MIMEType: "application/pdf", VersionID: "pending-no-job"})
	f.SeedDocumentAttachments(f.CreateMessage("backfill"), documents)
	for i, outcome := range []string{"provider_timeout", "consent", "stale_authority", "pdf_password_required", "not_found", "rendition_operator_required", "operator_required"} {
		d := store.DocbankAttachmentProcessing{ContentHash: fmt.Sprintf("%064x", 0xabcdef+i), VersionID: outcome, Fingerprint: "fp", Outcome: outcome, JobID: fmt.Sprintf("%064x", i+1)}
		if outcome == "not_found" || outcome == "rendition_operator_required" {
			d.JobID = ""
		}
		require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", d, time.Time{}))
	}
	pendingHash := fmt.Sprintf("%064x", 0xabcdf6)
	pendingJobID := fmt.Sprintf("%064x", 0x5678)
	require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", store.DocbankAttachmentProcessing{
		ContentHash: pendingHash, VersionID: "pending", Fingerprint: "fp", Outcome: "requested", JobID: pendingJobID,
	}, time.Now().Add(time.Hour)))
	noJobHash := fmt.Sprintf("%064x", 0xabcdf7)
	require.NoError(f.Store.RecordDocbankAttachmentProcessing(ctx, "attachments", store.DocbankAttachmentProcessing{
		ContentHash: noJobHash, Fingerprint: "fp", Outcome: "",
	}, time.Now().Add(time.Hour)))
	require.NoError(f.Store.RetryDocbankAttachments(ctx, "attachments"))
	due, err := f.Store.DocbankAttachmentsToProcess(ctx, "attachments", "fp", new(store.DocbankAttachmentProcessingCursor), 20)
	require.NoError(err)
	require.Len(due, 5)
	assert.ElementsMatch([]string{"consent", "stale_authority", "pdf_password_required", "rendition_operator_required", "operator_required"}, []string{due[0].VersionID, due[1].VersionID, due[2].VersionID, due[3].VersionID, due[4].VersionID})
	counts, err := f.Store.DocbankAttachmentProcessingCounts(ctx, "attachments", "documents")
	require.NoError(err)
	assert.Equal(int64(6), counts.Waiting)
	assert.Equal(int64(1), counts.Requested)
	assert.Equal(map[string]int64{
		"provider_timeout": 1,
		"not_found":        1,
	}, counts.Outcomes)
	var jobID string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT job_id FROM docbank_attachment_processing WHERE content_hash = ?`), pendingHash).Scan(&jobID))
	assert.Equal(pendingJobID, jobID)
	var next time.Time
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT job_id, next_action_at FROM docbank_attachment_processing WHERE content_hash = ?`), noJobHash).Scan(&jobID, &next))
	assert.Empty(jobID)
	assert.True(next.After(time.Now()))
}
