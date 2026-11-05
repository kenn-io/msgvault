package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAttachmentBlobReferencedUsesIndexes(t *testing.T) {
	st := openTestStore(t)

	for _, tc := range []struct {
		name, hash, path string
		indexes          []string
	}{
		{"hash", "orphan-hash", "", []string{"idx_attachments_hash", "idx_attachments_thumbnail_hash"}},
		{"path", "", "orphan/path", []string{"idx_attachments_storage_path", "idx_attachments_thumbnail_path"}},
		{"both", "orphan-hash", "orphan/path", []string{"idx_attachments_hash", "idx_attachments_thumbnail_hash", "idx_attachments_storage_path", "idx_attachments_thumbnail_path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, args := attachmentBlobReferenceQuery(tc.hash, tc.path)
			plan := explainPlan(t, st, query, args...)
			for _, index := range tc.indexes {
				assert.Contains(t, plan, "SEARCH attachments USING INDEX "+index+" ", "blob checks must seek each reference index:\n%s", plan)
			}
			assert.NotContains(t, plan, "SCAN attachments", "blob checks must not scan attachments per file:\n%s", plan)
		})
	}
}
