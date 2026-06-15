//go:build pgvector

package pgvector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestBackendSearchStructuredFilters(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})

	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `
		UPDATE messages
		   SET source_id = CASE id WHEN 1 THEN 10 WHEN 2 THEN 20 ELSE 30 END,
		       has_attachments = (id = 2),
		       size_estimate = CASE id WHEN 1 THEN 100 WHEN 2 THEN 200 ELSE 300 END,
		       sent_at = CASE id
		           WHEN 1 THEN $1::timestamptz
		           WHEN 2 THEN $2::timestamptz
		           ELSE $3::timestamptz
		       END
		 WHERE id IN (1, 2, 3)`,
		base, base.Add(time.Hour), base.Add(2*time.Hour))
	require.NoError(t, err, "seed message filter columns")

	_, err = db.ExecContext(ctx, `
		INSERT INTO message_recipients (message_id, recipient_type, participant_id) VALUES
			(2, 'from', 100),
			(2, 'to', 200),
			(3, 'cc', 300),
			(2, 'bcc', 400)`)
	require.NoError(t, err, "seed recipient rows")

	_, err = db.ExecContext(ctx,
		`INSERT INTO message_labels (message_id, label_id) VALUES (2, 42), (3, 43)`)
	require.NoError(t, err, "seed label rows")

	yes := true
	after := base.Add(30 * time.Minute)
	before := base.Add(90 * time.Minute)
	largerThan := int64(150)
	smallerThan := int64(250)

	tests := []struct {
		name   string
		filter vector.Filter
		want   []int64
	}{
		{
			name:   "sender group",
			filter: vector.Filter{SenderGroups: [][]int64{{100}}},
			want:   []int64{2},
		},
		{
			name:   "to group",
			filter: vector.Filter{ToGroups: [][]int64{{200}}},
			want:   []int64{2},
		},
		{
			name:   "cc group",
			filter: vector.Filter{CcGroups: [][]int64{{300}}},
			want:   []int64{3},
		},
		{
			name:   "bcc group",
			filter: vector.Filter{BccGroups: [][]int64{{400}}},
			want:   []int64{2},
		},
		{
			name:   "label group",
			filter: vector.Filter{LabelGroups: [][]int64{{42}}},
			want:   []int64{2},
		},
		{
			name:   "has attachment",
			filter: vector.Filter{HasAttachment: &yes},
			want:   []int64{2},
		},
		{
			name:   "date range",
			filter: vector.Filter{After: &after, Before: &before},
			want:   []int64{2},
		},
		{
			name:   "size range",
			filter: vector.Filter{LargerThan: &largerThan, SmallerThan: &smallerThan},
			want:   []int64{2},
		},
		{
			name:   "no match sentinel",
			filter: vector.Filter{SenderGroups: [][]int64{{-1}}},
			want:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, tc.filter)
			require.NoError(t, err, "Search")
			got := hitMessageIDs(hits)
			// Search returns (nil, nil) for an empty result, but the
			// hitMessageIDs helper materializes a non-nil empty slice.
			// Treat nil and empty as equivalent (matching the sqlitevec
			// sentinel precedent, fused_test.go's assert.Empty) instead
			// of asserting strict nil-vs-empty equality.
			if len(tc.want) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func hitMessageIDs(hits []vector.Hit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.MessageID
	}
	return out
}
