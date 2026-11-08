package query

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSenderSearch(t *testing.T) {
	env := newTestEnv(t)
	alex := env.AddParticipant(dbtest.ParticipantOpts{Email: new("alex@example.com"), DisplayName: new("Alex Kim")})
	bob := env.AddParticipant(dbtest.ParticipantOpts{Email: new("jimbob@example.com"), DisplayName: new("Jim Bob")})
	for range 4 {
		env.AddMessage(dbtest.MessageOpts{FromID: alex})
	}
	env.AddMessage(dbtest.MessageOpts{FromID: bob})
	decoy := env.AddParticipant(dbtest.ParticipantOpts{Email: new("alex@example.company")})
	env.AddMessage(dbtest.MessageOpts{FromID: decoy})
	header := env.AddParticipant(dbtest.ParticipantOpts{Email: new("other@elsewhere.test"), DisplayName: new("Participant Name")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: header})
	env.SetFromName(id, `Header_%\name`)
	env.AddMessage(dbtest.MessageOpts{FromID: header})
	phone := env.AddParticipant(dbtest.ParticipantOpts{Phone: new("+12025550123")})
	env.AddMessage(dbtest.MessageOpts{FromID: phone})
	for _, tc := range []struct {
		input string
		want  []int64
	}{
		{"from:alex", []int64{100, 101, 102, 103, 105}},
		{"from:kim", []int64{100, 101, 102, 103}},
		{"from:alex@example.com", []int64{100, 101, 102, 103}},
		{"from:bob@example.com", nil},
		{"from:example.com", []int64{1, 2, 3, 100, 101, 102, 103, 104}},
		{"from:@example.com", []int64{1, 2, 3, 100, 101, 102, 103, 104}},
		{"from:participant", []int64{106, 107}},
		{`from:header_%\name`, []int64{106}},
		{"from:5550123", []int64{108}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, err := env.Engine.Search(t.Context(), search.Parse(tc.input), 100, 0)
			require.NoError(err)
			ids := make([]int64, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			assert.ElementsMatch(tc.want, ids)
			fast, err := env.Engine.SearchFastWithStats(t.Context(), search.Parse(tc.input), tc.input, MessageFilter{}, ViewNoStats, 100, 0)
			require.NoError(err)
			assert.Equal(int64(len(tc.want)), fast.TotalCount)
		})
	}
}

func TestDuckDBSenderSearch(t *testing.T) {
	b := NewTestDataBuilder(t)
	b.AddSource("reader@example.org")
	alex := b.AddParticipant("alex@example.com", "example.com", "Alex Kim")
	bob := b.AddParticipant("jimbob@example.com", "example.com", "Jim Bob")
	decoy := b.AddParticipant("alex@example.company", "example.company", "Decoy")
	header := b.AddParticipant("other@elsewhere.test", "elsewhere.test", "Participant Name")
	phone := b.AddPhoneParticipant("+12025550123", "")
	for i, sender := range []int64{alex, alex, alex, alex, bob, decoy, header, header, phone} {
		id := b.AddMessage(MessageOpt{Subject: "senderfixture"})
		name := ""
		if i == 6 {
			name = `Header_%\name`
		}
		b.AddFrom(id, sender, name)
	}
	engine := b.BuildEngine()
	for _, tc := range []struct {
		input string
		want  []int64
	}{
		{"from:alex", []int64{1, 2, 3, 4, 6}},
		{"from:kim", []int64{1, 2, 3, 4}},
		{"from:alex@example.com", []int64{1, 2, 3, 4}},
		{"from:bob@example.com", nil},
		{"from:example.com", []int64{1, 2, 3, 4, 5}},
		{"from:@example.com", []int64{1, 2, 3, 4, 5}},
		{"from:participant", []int64{7, 8}},
		{`from:header_%\name`, []int64{7}},
		{"from:5550123", []int64{9}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, err := engine.SearchFast(t.Context(), search.Parse(tc.input), MessageFilter{}, 100, 0)
			require.NoError(err)
			ids := make([]int64, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			assert.ElementsMatch(tc.want, ids)
			conditions, args := engine.buildNonTextSearchConditions(search.Parse(tc.input))
			rows2, err := engine.db.QueryContext(t.Context(), "WITH "+engine.parquetCTEs()+" SELECT msg.id FROM msg WHERE "+strings.Join(conditions, " AND "), args...)
			require.NoError(err)
			defer func() { require.NoError(rows2.Close()) }()
			ids = ids[:0]
			for rows2.Next() {
				var id int64
				require.NoError(rows2.Scan(&id))
				ids = append(ids, id)
			}
			require.NoError(rows2.Err())
			assert.ElementsMatch(tc.want, ids)
		})
	}
}
