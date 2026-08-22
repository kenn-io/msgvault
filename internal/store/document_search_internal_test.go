package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFuseDocumentSearchRowsCapsCombinedSignals(t *testing.T) {
	contentRows := []documentSearchRow{
		{OccurrenceKey: "content-1", AttachmentID: 1, ContentRank: 1},
		{OccurrenceKey: "content-2", AttachmentID: 2, ContentRank: 2},
	}
	filenameRows := []documentSearchRow{
		{OccurrenceKey: "filename-1", AttachmentID: 3, FilenameRank: 1},
		{OccurrenceKey: "filename-2", AttachmentID: 4, FilenameRank: 2},
	}

	rows, truncated := fuseDocumentSearchRows(contentRows, filenameRows, nil, 3)

	require.Len(t, rows, 3)
	assert.True(t, truncated)
}
