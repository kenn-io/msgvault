//go:build pgvector

package pgvector

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestVectorFilterMessageIDsBeforeRanking(t *testing.T) {
	f := seedThree(t)
	pure, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10,
		vector.Filter{MessageIDs: []int64{2}})
	require.NoError(t, err)
	require.Len(t, pure, 1)
	assert.Equal(t, int64(2), pure[0].MessageID)

	fused, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSTerms: []string{"quantum"}, QueryVec: unitVec(4, 0), Generation: f.gen,
		KPerSignal: 10, Limit: 10, RRFK: 60, Filter: vector.Filter{MessageIDs: []int64{2}},
	})
	require.NoError(t, err)
	require.Len(t, fused, 1)
	assert.Equal(t, int64(2), fused[0].MessageID)

	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	clauses := buildPGFilterClauses(vector.Filter{MessageIDs: []int64{7, 11}}, bind)
	require.Len(t, clauses, 1)
	assert.Equal(t, "m.id = ANY($1::bigint[])", clauses[0])
	assert.Equal(t, []any{`{7,11}`}, args)

	tooMany := make([]int64, vector.MaxFilterMessageIDs+1)
	_, err = f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10, vector.Filter{MessageIDs: tooMany})
	require.ErrorIs(t, err, vector.ErrFilterTooLarge)
	_, _, err = f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSTerms: []string{"quantum"}, Generation: f.gen, KPerSignal: 10, Limit: 10,
		Filter: vector.Filter{MessageIDs: tooMany},
	})
	require.ErrorIs(t, err, vector.ErrFilterTooLarge)
}

// TestVectorFilterListIDsBeforeRanking catches semantic and fused candidate
// queries admitting messages whose List-Id does not satisfy every literal term.
func TestVectorFilterListIDsBeforeRanking(t *testing.T) {
	f := seedThree(t)
	_, err := f.db.ExecContext(f.ctx, `UPDATE messages SET list_id = CASE id
		WHEN 1 THEN '<Announce.Shared.example.org>'
		WHEN 2 THEN '<ÉCOLE.example.org>'
		WHEN 3 THEN '<Ops%_Team\Archive.example.org>'
	END WHERE id IN (1, 2, 3)`)
	require.NoError(t, err, "seed list ids")

	filter := vector.Filter{ListIDSubstrings: []string{"ANNOUNCE", "shared"}}
	pure, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10, filter)
	require.NoError(t, err)
	require.Len(t, pure, 1)
	assert.Equal(t, int64(1), pure[0].MessageID)

	fused, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec: unitVec(4, 0), Generation: f.gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: filter,
	})
	require.NoError(t, err)
	require.Len(t, fused, 1)
	assert.Equal(t, int64(1), fused[0].MessageID)

	exactFilter := vector.Filter{ListID: "<école.example.org>"}
	exact, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10, exactFilter)
	require.NoError(t, err)
	require.Len(t, exact, 1)
	assert.Equal(t, int64(2), exact[0].MessageID)

	exactFused, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec: unitVec(4, 0), Generation: f.gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: exactFilter,
	})
	require.NoError(t, err)
	require.Len(t, exactFused, 1)
	assert.Equal(t, int64(2), exactFused[0].MessageID)

	exactGroups := vector.Filter{ListIDExactGroups: [][]string{
		{"<Announce.Shared.example.org>", "<ÉCOLE.example.org>"},
		{"<ANNOUNCE.SHARED.EXAMPLE.ORG>", "<missing.example.org>"},
	}}
	groupHits, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10, exactGroups)
	require.NoError(t, err)
	require.Len(t, groupHits, 1)
	assert.Equal(t, int64(1), groupHits[0].MessageID)

	groupFused, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec: unitVec(4, 0), Generation: f.gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: exactGroups,
	})
	require.NoError(t, err)
	require.Len(t, groupFused, 1)
	assert.Equal(t, int64(1), groupFused[0].MessageID)

	literal, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10,
		vector.Filter{ListIDSubstrings: []string{"ops%_team"}})
	require.NoError(t, err)
	require.Len(t, literal, 1)
	assert.Equal(t, int64(3), literal[0].MessageID)

	backslash, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10,
		vector.Filter{ListIDSubstrings: []string{"ops%_team\\archive"}})
	require.NoError(t, err)
	require.Len(t, backslash, 1)
	assert.Equal(t, int64(3), backslash[0].MessageID)
	backslashFused, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec: unitVec(4, 0), Generation: f.gen, KPerSignal: 10, Limit: 10, RRFK: 60,
		Filter: vector.Filter{ListIDSubstrings: []string{"ops%_team\\archive"}},
	})
	require.NoError(t, err)
	require.Len(t, backslashFused, 1)
	assert.Equal(t, int64(3), backslashFused[0].MessageID)

	unicodeFilter := vector.Filter{ListIDSubstrings: []string{"école"}}
	unicode, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 10, unicodeFilter)
	require.NoError(t, err)
	require.Len(t, unicode, 1)
	assert.Equal(t, int64(2), unicode[0].MessageID)

	unicodeFused, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec: unitVec(4, 0), Generation: f.gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: unicodeFilter,
	})
	require.NoError(t, err)
	require.Len(t, unicodeFused, 1)
	assert.Equal(t, int64(2), unicodeFused[0].MessageID)
}

func TestBuildPGFilterClausesExactListID(t *testing.T) {
	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	clauses := buildPGFilterClauses(vector.Filter{ListID: "<Dev@Example.Test>"}, bind)

	require.Equal(t, []string{"LOWER(m.list_id) = LOWER($1)"}, clauses)
	assert.Equal(t, []any{"<Dev@Example.Test>"}, args)
}

func TestBuildPGFilterClausesMessageTypes(t *testing.T) {
	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	clauses := buildPGFilterClauses(vector.Filter{MessageTypes: []string{"sms", "mms"}}, bind)

	require.Len(t, clauses, 1)
	assert.Equal(t, "(m.message_type = ANY($1::text[]))", clauses[0])
	assert.Equal(t, []any{`{"sms","mms"}`}, args)
}

func TestBuildPGFilterClausesConversationIDs(t *testing.T) {
	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	clauses := buildPGFilterClauses(vector.Filter{ConversationIDs: []int64{7, 11}}, bind)

	require.Len(t, clauses, 1)
	assert.Equal(t, "m.conversation_id = ANY($1::bigint[])", clauses[0])
	assert.Equal(t, []any{`{7,11}`}, args)
}

func TestBuildPGFilterClausesLegacyEmail(t *testing.T) {
	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	clauses := buildPGFilterClauses(vector.Filter{MessageTypes: []string{"email"}}, bind)

	require.Len(t, clauses, 1)
	assert.Contains(t, clauses[0], "m.message_type IS NULL")
	assert.Contains(t, clauses[0], "m.message_type = ''")
	assert.Equal(t, []any{"email"}, args)
}

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
		       message_type = CASE id WHEN 1 THEN 'email' WHEN 2 THEN 'sms' ELSE 'mms' END,
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
			name:   "message type",
			filter: vector.Filter{MessageTypes: []string{"sms"}},
			want:   []int64{2},
		},
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
			name:   "recipient any group",
			filter: vector.Filter{RecipientAnyGroups: [][]int64{{300}}},
			want:   []int64{3},
		},
		{
			name:   "exact sender group",
			filter: vector.Filter{SenderExactGroups: [][]int64{{100}}},
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
			name:   "message type",
			filter: vector.Filter{MessageTypes: []string{"sms"}},
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
			name:   "message type",
			filter: vector.Filter{MessageTypes: []string{"sms"}},
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

func TestBackendSearchMessageTypeFilter(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	_, err := db.ExecContext(ctx, `UPDATE messages SET message_type = CASE id WHEN 1 THEN 'email' ELSE 'sms' END`)
	require.NoError(t, err, "seed message_type")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{MessageTypes: []string{"sms"}})
	require.NoError(t, err, "Search")
	assert.Equal(t, []int64{2, 3}, hitMessageIDs(hits))
}

func hitMessageIDs(hits []vector.Hit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.MessageID
	}
	return out
}
