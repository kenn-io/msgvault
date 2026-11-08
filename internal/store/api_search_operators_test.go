package store_test

import (
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
	alex := f.EnsureParticipant("alex@example.com", "Alex Kim", "example.com")
	jim := f.EnsureParticipant("jimbob@example.com", "Jim Bob", "example.com")
	domainDecoy := f.EnsureParticipant("alex@example.company", "Decoy", "example.company")
	ids := f.CreateMessages(40)
	for i, sender := range []int64{alice, bob, literal, decoy} {
		requirements.NoError(f.Store.ReplaceMessageRecipients(ids[i], "from", []int64{sender}, nil))
	}
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[0], "to", []int64{bob, carol}, nil))
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[1], "to", []int64{carol}, nil))
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[1], "cc", []int64{bob}, nil))
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[0], "bcc", []int64{carol}, nil))
	labels := f.EnsureLabels(map[string]string{"work": "Work", "literal": "work_100%", "decoy": "workX100Z"}, "user")
	requirements.NoError(f.Store.AddMessageLabels(t.Context(), ids[0], []int64{labels["work"], labels["literal"]}))
	requirements.NoError(f.Store.AddMessageLabels(t.Context(), ids[1], []int64{labels["decoy"]}))

	for i, sender := range []int64{alex, alex, alex, alex, jim, domainDecoy} {
		requirements.NoError(f.Store.ReplaceMessageRecipients(ids[4+i], "from", []int64{sender}, nil))
	}
	requirements.NoError(f.Store.ReplaceMessageRecipients(ids[10], "from", []int64{jim}, []string{`Header_%\name`}))
	for _, tc := range []struct {
		input string
		want  []int64
	}{
		{"from:alex", []int64{ids[4], ids[5], ids[6], ids[7], ids[9]}},
		{"from:kim", []int64{ids[4], ids[5], ids[6], ids[7]}},
		{"from:alex@example.com", []int64{ids[4], ids[5], ids[6], ids[7]}},
		{"from:bob@example.com", []int64{ids[1]}},
		{"from:example.com", []int64{ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8], ids[10]}},
		{"from:@example.com", []int64{ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8], ids[10]}},
		{`from:header_%\name`, []int64{ids[10]}},
		{"from:alice", []int64{ids[0]}},
		{"from:ALICE@EXAMPLE.COM", []int64{ids[0]}},
		{"to:bob", []int64{ids[0]}},
		{"to:bob to:carol", []int64{ids[0]}},
		{"cc:bob", []int64{ids[1]}},
		{"bcc:carol", []int64{ids[0]}},
		{"from:alice | from:bob", []int64{ids[0], ids[1], ids[8], ids[10]}},
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
