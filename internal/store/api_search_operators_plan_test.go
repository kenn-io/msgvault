package store

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestSearchOperatorsLookupPlan(t *testing.T) {
	require := require.New(t)
	checks := assert.New(t)
	path := filepath.Join(t.TempDir(), "operators.db")
	s, err := Open(path)
	require.NoError(err)
	require.NoError(s.InitSchema())
	seedLiveMessages(t, s, 2000)
	_, err = s.db.Exec(`UPDATE messages SET deleted_from_source_at = NULL`)
	require.NoError(err)
	for _, stmt := range []string{
		`INSERT INTO participants (id, email_address) VALUES (2, 'rare@example.com'), (3, 'broad@example.com'), (4, 'tenth@example.com')`,
		`INSERT INTO labels (id, source_id, source_label_id, name) VALUES (1, 1, 'rare', 'rare'), (2, 1, 'broad', 'broad')`,
		`INSERT INTO message_labels (message_id, label_id) SELECT id, 2 FROM messages`,
		`INSERT INTO message_labels (message_id, label_id) VALUES (1, 1)`,
	} {
		_, err = s.db.Exec(stmt)
		require.NoError(err)
	}
	for _, role := range []string{"from", "to", "cc", "bcc"} {
		for _, spec := range []struct{ participant, count int }{{2, 1}, {3, 2000}, {4, 200}} {
			_, err = s.db.Exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) SELECT id, ?, ? FROM messages WHERE id <= ?`, spec.participant, role, spec.count)
			require.NoError(err)
		}
	}
	// More than 250 matching associations on one message must still use a lookup.
	for id := 5; id <= 255; id++ {
		_, err = s.db.Exec(`INSERT INTO participants (id, email_address) VALUES (?, ?)`, id, fmt.Sprintf("fanout%d@example.com", id))
		require.NoError(err)
		_, err = s.db.Exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (1, ?, 'to')`, id)
		require.NoError(err)
		_, err = s.db.Exec(`INSERT INTO labels (id, source_id, source_label_id, name) VALUES (?, 1, ?, ?)`, id, strconv.Itoa(id), fmt.Sprintf("fanout%d", id))
		require.NoError(err)
		_, err = s.db.Exec(`INSERT INTO message_labels (message_id, label_id) VALUES (1, ?)`, id)
		require.NoError(err)
	}
	require.NoError(s.Close())
	s, err = Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(s.Close()) })
	for _, op := range []string{"from", "to", "cc", "bcc", "label"} {
		for _, value := range []string{"rare", "absent"} {
			if op == "from" {
				value += "@example.com"
			}
			t.Run(op+":"+value, func(t *testing.T) {
				checks := assert.New(t)
				plan := s.buildMessageSearchSQL(t.Context(), search.Parse(op+":"+value), true)
				for _, selectSQL := range []string{"SELECT COUNT(*)", "SELECT m.id"} {
					sql := selectSQL + " FROM messages m " + plan.join + " WHERE " + plan.where
					if selectSQL == "SELECT m.id" {
						sql += " ORDER BY " + plan.orderBy + " LIMIT 50"
					}
					explained := explainPlan(t, s, sql, plan.args...)
					checks.NotRegexp(`SCAN m\b`, explained)
					checks.NotContains(explained, "SCAN mr2", explained)
				}
			})
		}
		value := "broad"
		if op == "from" {
			value += "@example.com"
		}
		plan := s.buildMessageSearchSQL(t.Context(), search.Parse(op+":"+value), true)
		checks.NotContains(plan.where, "m.id IN (SELECT", op)
	}
	for _, input := range []string{"to:fanout", "label:fanout"} {
		t.Run(input, func(t *testing.T) {
			plan := s.buildMessageSearchSQL(t.Context(), search.Parse(input), true)
			assert.Contains(t, plan.where, "m.id IN (SELECT")
		})
	}
	for _, tc := range []struct {
		input           string
		lookupCondition int
	}{
		{"from:rare@example.com to:broad", 0},
		{"from:rare@example.com to:tenth", 0},
		{"from:tenth@example.com to:rare", 1},
	} {
		input := tc.input
		plan := s.buildMessageSearchSQL(t.Context(), search.Parse(input), true)
		checks.Contains(plan.conditions[1+tc.lookupCondition], "m.id IN (SELECT", input)
		checks.Equal(1, strings.Count(plan.where, "m.id IN (SELECT"), input)
		checks.Contains(plan.where, "EXISTS (", input)
		explained := explainPlan(t, s, "SELECT COUNT(*) FROM messages m WHERE "+plan.where, plan.args...)
		checks.NotRegexp(`SCAN m\b`, explained)
	}
	for _, input := range []string{"from:rare invoice", "from:rare conversation_id:1", "from:rare after:2020-01-01", "from:rare message_type:email", "from:rare | to:broad"} {
		plan := s.buildMessageSearchSQL(t.Context(), search.ParseOR(input), true)
		checks.NotContains(plan.where, "m.id IN (SELECT", input)
	}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{1})
	plan := s.buildMessageSearchSQL(ctx, search.Parse("from:rare"), true)
	checks.NotContains(plan.where, "m.id IN (SELECT")
}
