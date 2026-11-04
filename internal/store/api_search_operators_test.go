package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchOperatorsPreserveResults(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	alice := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	bob := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	carol := f.EnsureParticipant("carol@example.com", "Carol", "example.com")
	literal := f.EnsureParticipant("a_lice%tag@example.com", "Literal", "example.com")
	decoy := f.EnsureParticipant("axliceZtag@example.com", "Decoy", "example.com")
	ids := f.CreateMessages(40)
	for i, sender := range []int64{alice, bob, literal, decoy} {
		requirements.NoError(f.Store.ReplaceMessageRecipients(ids[i], "from", []int64{sender}, nil))
	}
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[0], "to", []int64{bob, carol}, nil))
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[1], "to", []int64{carol}, nil))
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[1], "cc", []int64{bob}, nil))
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[0], "bcc", []int64{carol}, nil))
	labels := f.EnsureLabels(map[string]string{"work": "Work", "literal": "work_100%", "decoy": "workX100Z"}, "user")
	requirements.NoError(f.Store.AddMessageLabels(ids[0], []int64{labels["work"], labels["literal"]}))
	requirements.NoError(f.Store.AddMessageLabels(ids[1], []int64{labels["decoy"]}))

	for _, tc := range []struct {
		input string
		want  []int64
	}{
		{"from:alice", []int64{ids[0]}},
		{"from:ALICE@EXAMPLE.COM", []int64{ids[0]}},
		{"to:bob", []int64{ids[0]}},
		{"to:bob to:carol", []int64{ids[0]}},
		{"cc:bob", []int64{ids[1]}},
		{"bcc:carol", []int64{ids[0]}},
		{"from:alice | from:bob", []int64{ids[0], ids[1]}},
		{"label:wor", []int64{ids[0], ids[1]}},
		{"from:a_lice", []int64{ids[2]}},
		{"from:lice%tag", []int64{ids[2]}},
		{"label:work_", []int64{ids[0]}},
		{"label:100%", []int64{ids[0]}},
		{"to:absent", []int64{}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			q := search.ParseOR(tc.input)
			rows, total, err := f.Store.SearchMessagesQueryContext(t.Context(), q, 0, 50)
			require.NoError(err)
			got := make([]int64, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.ID)
			}
			assert.ElementsMatch(tc.want, got)
			assert.Equal(int64(len(tc.want)), total)
			candidates, total, err := f.Store.SearchMessageIDsQueryContext(t.Context(), q, 50)
			require.NoError(err)
			assert.ElementsMatch(tc.want, candidates)
			assert.Equal(int64(len(tc.want)), total)
		})
	}
}

func TestSearchOperatorsShareBoundaryResults(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ids := f.CreateMessages(80)
	for _, count := range []int{9, 10, 11} {
		addr := fmt.Sprintf("boundary%d@example.com", count)
		participant := f.EnsureParticipant(addr, "Boundary", "example.com")
		for _, id := range ids[:count] {
			require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{participant}, nil))
		}
		q := search.Parse("to:" + addr)
		rows, total, err := f.Store.SearchMessagesQueryContext(t.Context(), q, 0, 80)
		require.NoError(err)
		got := make([]int64, 0, len(rows))
		for _, row := range rows {
			got = append(got, row.ID)
		}
		assert.ElementsMatch(ids[:count], got)
		assert.Equal(int64(count), total)
		candidates, total, err := f.Store.SearchMessageIDsQueryContext(t.Context(), q, 80)
		require.NoError(err)
		assert.ElementsMatch(ids[:count], candidates)
		assert.Equal(int64(count), total)
	}
}
