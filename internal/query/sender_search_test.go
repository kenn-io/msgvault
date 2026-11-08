package query

import (
	"path/filepath"
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
	header := env.AddParticipant(dbtest.ParticipantOpts{Email: new("other@elsewhere.test"), DisplayName: new("Élodie Participant")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: header})
	env.SetFromName(id, `Álvaro Header_%\name`)
	env.AddMessage(dbtest.MessageOpts{FromID: header})
	phone := env.AddParticipant(dbtest.ParticipantOpts{Phone: new("+12025550123")})
	env.AddMessage(dbtest.MessageOpts{FromID: phone})
	chat := env.AddParticipant(dbtest.ParticipantOpts{Phone: new("+12025550999"), DisplayName: new("Alex Chat")})
	chatID := env.AddMessage(dbtest.MessageOpts{})
	_, err := env.DB.ExecContext(t.Context(), `UPDATE messages SET sender_id = ? WHERE id = ?`, chat, chatID)
	require.NoError(t, err)
	for _, address := range []string{"root@localhost", "root@localhost.example"} {
		sender := env.AddParticipant(dbtest.ParticipantOpts{Email: &address})
		id := env.AddMessage(dbtest.MessageOpts{})
		_, err := env.DB.ExecContext(t.Context(), `UPDATE messages SET sender_id = ? WHERE id = ?`, sender, id)
		require.NoError(t, err)
	}
	cases := []struct {
		input string
		want  []int64
	}{
		{"from:alex", []int64{100, 101, 102, 103, 105, 109}},
		{"from:kim", []int64{100, 101, 102, 103}},
		{"from:élodie", []int64{106, 107}}, {"from:álvaro", []int64{106}},
		{"from:alex@", []int64{100, 101, 102, 103, 105}},
		{"from:alex@exam", nil},
		{"from:alex@example.com from:jimbob@example.com", nil},
		{"from:alex@example.com", []int64{100, 101, 102, 103}},
		{"from:bob@example.com", nil},
		{"from:example.com", []int64{1, 2, 3, 100, 101, 102, 103, 104}},
		{"from:@example.com", []int64{1, 2, 3, 100, 101, 102, 103, 104}},
		{"from:participant", []int64{106, 107}},
		{`from:header_%\name`, []int64{106}},
		{"from:5550123", []int64{108}},
		{"from:chat", []int64{109}}, {"from:5550999", []int64{109}},
		{"from:root@localhost", []int64{110}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, err := env.Engine.Search(t.Context(), search.Parse(tc.input), 100, 0)
			require.NoError(err)
			ids := make([]int64, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			assert.ElementsMatch(tc.want, ids)
			if tc.input == "from:alex" {
				fast, err := env.Engine.SearchFastWithStats(t.Context(), search.Parse(tc.input), tc.input, MessageFilter{}, ViewNoStats, 100, 0)
				require.NoError(err)
				assert.Equal(int64(len(tc.want)), fast.TotalCount)
			}
		})
	}
	t.Run("scanner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sender.db")
		_, err := env.DB.ExecContext(t.Context(), "VACUUM INTO ?", path)
		require.NoError(t, err)
		engine, err := NewDuckDBEngine(t.Context(), "", path, nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, engine.Close()) })
		if !engine.hasSQLite() {
			t.Skip("DuckDB sqlite_scanner extension unavailable")
		}
		for _, tc := range cases {
			switch tc.input {
			case "from:élodie", "from:álvaro", `from:header_%\name`, "from:example.com", "from:chat":
			default:
				continue
			}
			t.Run(tc.input, func(t *testing.T) {
				rows, err := engine.Search(t.Context(), search.Parse(tc.input), 100, 0)
				require.NoError(t, err)
				var ids []int64
				for _, row := range rows {
					ids = append(ids, row.ID)
				}
				assert.ElementsMatch(t, tc.want, ids)
			})
		}
	})
}

func TestDuckDBSenderSearch(t *testing.T) {
	b := NewTestDataBuilder(t)
	b.AddSource("reader@example.org")
	alex := b.AddParticipant("alex@example.com", "example.com", "Alex Kim")
	bob := b.AddParticipant("jimbob@example.com", "example.com", "Jim Bob")
	decoy := b.AddParticipant("alex@example.company", "example.company", "Decoy")
	header := b.AddParticipant("other@elsewhere.test", "elsewhere.test", "Élodie Participant")
	phone := b.AddPhoneParticipant("+12025550123", "")
	for i, sender := range []int64{alex, alex, alex, alex, bob, decoy, header, header, phone} {
		id := b.AddMessage(MessageOpt{Subject: "senderfixture"})
		name := ""
		if i == 6 {
			name = `Álvaro Header_%\name`
		}
		b.AddFrom(id, sender, name)
	}
	chat := b.AddPhoneParticipant("+12025550999", "Alex Chat")
	b.AddMessage(MessageOpt{SenderID: &chat, Subject: "senderfixture"})
	for _, address := range []string{"root@localhost", "root@localhost.example"} {
		sender := b.AddParticipant(address, "localhost", "Root")
		b.AddMessage(MessageOpt{SenderID: &sender, Subject: "senderfixture"})
	}
	engine := b.BuildEngine()
	for _, tc := range []struct {
		input string
		want  []int64
	}{
		{"from:alex", []int64{1, 2, 3, 4, 6, 10}},
		{"from:kim", []int64{1, 2, 3, 4}},
		{"from:élodie", []int64{7, 8}}, {"from:álvaro", []int64{7}},
		{"from:alex@", []int64{1, 2, 3, 4, 6}},
		{"from:alex@exam", nil},
		{"from:alex@example.com from:jimbob@example.com", nil},
		{"from:alex@example.com", []int64{1, 2, 3, 4}},
		{"from:bob@example.com", nil},
		{"from:example.com", []int64{1, 2, 3, 4, 5}},
		{"from:@example.com", []int64{1, 2, 3, 4, 5}},
		{"from:participant", []int64{7, 8}},
		{`from:header_%\name`, []int64{7}},
		{"from:5550123", []int64{9}},
		{"from:chat", []int64{10}}, {"from:5550999", []int64{10}}, {"from:root@localhost", []int64{11}},
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
			if tc.input == "from:chat" {
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
			}
		})
	}
}

func TestSenderSearchWithinExactScope(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	env := newTestEnv(t)
	for _, number := range []string{"+12025550999", "+120255509990"} {
		participant := env.AddParticipant(dbtest.ParticipantOpts{Phone: &number, DisplayName: new("Alex Chat")})
		id := env.AddMessage(dbtest.MessageOpts{Subject: "scopefixture"})
		_, err := env.DB.ExecContext(t.Context(), `UPDATE messages SET sender_id = ? WHERE id = ?`, participant, id)
		require.NoError(err)
	}
	filter := MessageFilter{Sender: "+12025550999"}
	q := search.Parse("from:alex")
	fast, err := env.Engine.SearchFastWithStats(t.Context(), q, "from:alex", filter, ViewNoStats, 100, 0)
	require.NoError(err)
	require.Len(fast.Messages, 1)
	assert.Equal(int64(100), fast.Messages[0].ID)
	assert.Equal(int64(1), fast.TotalCount)
	deep, err := env.Engine.SearchDeepWithStats(t.Context(), q, filter, 100, 0)
	require.NoError(err)
	require.Len(deep.Messages, 1)
	assert.Equal(int64(100), deep.Messages[0].ID)
	assert.Equal(int64(1), deep.TotalCount)
}
