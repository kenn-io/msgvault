package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/filenamediscovery"
	"go.kenn.io/msgvault/internal/personscope"
)

// Observe native emitted rows, rather than inferring SQL work from the returned
// population. The extra tuple is a lookahead, never a partial success.
func filenameScopePlanFixture(t *testing.T) (*Store, *filenamePostingObserver, []int64, []int64) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	st, observer := filenameObservedStore(t)
	source, err := st.GetOrCreateSource("imap", "fixture@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "scope-plan", "Scope Example")
	require.NoError(err)
	tx, err := st.db.BeginTx(t.Context(), nil)
	require.NoError(err)
	var person int64
	require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO persons(vcard_uid) VALUES('native-scope') RETURNING id`).Scan(&person))
	var members, ids []int64
	for i := range 66 {
		var p int64
		require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO participants(email_address) VALUES(?) RETURNING id`, fmt.Sprintf("scope%03d@example.test", i)).Scan(&p))
		_, err = tx.ExecContext(t.Context(), `INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)`, person, p)
		require.NoError(err)
		members = append(members, p)
	}
	for i := range 65 {
		var m, id int64
		require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) VALUES(?,?,?,'email') RETURNING id`, source.ID, conv, strconv.Itoa(i)).Scan(&m))
		require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,'report.pdf','') RETURNING id`, m).Scan(&id))
		ids = append(ids, id)
		for _, p := range members[:64] {
			_, err = tx.ExecContext(t.Context(), `INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'to','member@example.test')`, m, p)
			require.NoError(err)
		}
	}
	for _, p := range members[:64] {
		_, err = tx.ExecContext(t.Context(), `INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)`, conv, p)
		require.NoError(err)
	}
	state := filenameDiscoveryState{Generation: 1, Revision: 1, Phase: "ready"}
	updates := make([]filenameProjectionUpdate, 0, len(ids))
	for _, id := range ids {
		updates = append(updates, filenameProjectionUpdate{id: id, currentName: "report.pdf", currentFound: true})
	}
	require.NoError(st.filenameApplyBatchTx(t.Context(), tx, &state, updates))
	require.NoError(saveFilenameDiscoveryState(t.Context(), tx, state))
	_, err = tx.ExecContext(t.Context(), `INSERT INTO attachment_change_consumers(consumer_key,baseline_sequence,last_sequence,reconciliation_complete) VALUES(?,0,0,TRUE)`, filenameDiscoveryConsumer)
	require.NoError(err)
	// Give the optimizer a real unrelated population, so native plan evidence
	// measures point probes rather than a cheaper scan of a one-page table.
	for i := range 2000 {
		var c, m int64
		require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,?,'email_thread') RETURNING id`, source.ID, fmt.Sprintf("unrelated-%d", i)).Scan(&c))
		require.NoError(tx.QueryRowContext(t.Context(), `INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) VALUES(?,?,?,'email') RETURNING id`, source.ID, c, fmt.Sprintf("unrelated-%d", i)).Scan(&m))
		_, err = tx.ExecContext(t.Context(), `INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'to','unrelated@example.test')`, m, members[64])
		require.NoError(err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)`, c, members[64])
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	for _, table := range []string{"message_recipients", "conversation_participants", "person_participants", "messages", "conversations"} {
		_, err = st.DB().Exec(`ANALYZE ` + table)
		require.NoError(err)
	}
	observer.all = true
	_, err = st.FilenamePersonContext(t.Context(), person)
	require.ErrorIs(err, ErrFilenameScopeTooLarge)
	var bindingRows int
	for _, q := range observer.queries {
		assert.NotContains(strings.ToLower(q.sql), "carddav")
		if strings.Contains(q.sql, "FROM person_participants") {
			bindingRows += q.rows
		}
	}
	assert.Equal(65, bindingRows)
	return st, observer, members, ids
}

func TestFilenamePersonScopePhysicalBoundsAndPlans(t *testing.T) {
	st, observer, members, ids := filenameScopePlanFixture(t)
	for _, tc := range []struct {
		name         string
		membership   bool
		participants int
		edges        int
	}{
		{"directions-64", false, 64, 4097},
		{"membership-64", true, 64, 4097},
		{"membership-1", true, 1, 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			observer.all = true
			observer.queries = nil
			scope := personscope.Scope{ParticipantIDs: members[:tc.participants], Directions: []personscope.Direction{personscope.FromPerson, personscope.ToPerson, personscope.Group}, IncludeUnclassifiedRosterRows: true, ParticipantMembership: tc.membership}
			q, err := filenamediscovery.ParseQuery("report.pdf")
			require.NoError(err)
			candidates, err := st.FilenameCandidatesContext(t.Context(), q, FilenameSuggestionScope{Person: &scope})
			require.NoError(err)
			assert.ElementsMatch(ids, candidates.IDs, "every fixture message names a scoped participant")
			rows, err := st.FilenameSuggestionRowsContext(t.Context(), ids, FilenameSuggestionScope{Person: &scope})
			if tc.edges > filenameProvenanceBudget {
				require.ErrorIs(err, ErrFilenameScopeTooLarge)
				assert.Nil(rows)
			} else {
				require.NoError(err)
				assert.Len(rows, 65)
			}
			queries := append([]filenameObservedQuery(nil), observer.queries...)
			observer.all = false
			var edges int
			var planQuery *filenameObservedQuery
			for i, q := range queries {
				if strings.Contains(q.sql, "filename_edges") {
					edges += q.rows
					planQuery = &queries[i]
				}
			}
			assert.Equal(tc.edges, edges)
			require.NotNil(planQuery)
			var authorityQuery *filenameObservedQuery
			for i, q := range queries {
				if strings.Contains(q.sql, "FROM attachments a") {
					authorityQuery = &queries[i]
				}
			}
			require.NotNil(authorityQuery)
			var postingQuery *filenameObservedQuery
			for i, q := range queries {
				if strings.Contains(q.sql, "p.attachment_id") {
					postingQuery = &queries[i]
					break
				}
			}
			require.NotNil(postingQuery)
			for _, planQuery := range []*filenameObservedQuery{postingQuery, authorityQuery, planQuery} {
				if st.dialect.DriverName() == postgresDriverName {
					var raw string
					require.NoError(st.DB().QueryRowContext(t.Context(), `EXPLAIN (FORMAT JSON) `+planQuery.sql, planQuery.args...).Scan(&raw))
					var plan []map[string]any
					require.NoError(json.Unmarshal([]byte(raw), &plan))
					indexed := map[string]bool{}
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						kind, _ := node["Node Type"].(string)
						assert.NotEqual("Sort", kind)
						assert.NotEqual("Aggregate", kind)
						relation, _ := node["Relation Name"].(string)
						if relation == "message_recipients" || relation == "conversation_participants" {
							assert.Contains(kind, "Index", raw)
							condition, _ := node["Index Cond"].(string)
							key := "message_id"
							if relation == "conversation_participants" {
								key = "conversation_id"
							}
							assert.Contains(condition, key, "relation probes must seek by the bounded message or conversation, not scan a participant's whole archive")
							indexed[relation] = strings.Contains(kind, "Index")
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								childPlan, ok := child.(map[string]any)
								require.True(ok)
								inspect(childPlan)
							}
						}
					}
					require.NotEmpty(plan)
					root, ok := plan[0]["Plan"].(map[string]any)
					require.True(ok)
					inspect(root)
					assert.True(indexed["message_recipients"])
					assert.True(indexed["conversation_participants"])
					t.Logf("native bounded provenance emitted=%d PGplan=%s", edges, raw)
				} else {
					plan, err := st.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+planQuery.sql, planQuery.args...)
					require.NoError(err)
					defer func() { require.NoError(plan.Close()) }()
					var recipient, roster bool
					for plan.Next() {
						var id, parent, unused int
						var detail string
						require.NoError(plan.Scan(&id, &parent, &unused, &detail))
						assert.NotContains(detail, "TEMP B-TREE")
						if strings.Contains(detail, "mr ") || strings.Contains(detail, "known_from ") || strings.Contains(detail, "mr_from ") || strings.Contains(detail, "person_recipient ") || strings.Contains(detail, "person_from ") || strings.Contains(detail, "person_to ") || strings.Contains(detail, "scoped_sender ") {
							assert.Contains(detail, "SEARCH")
							recipient = true
						}
						if strings.Contains(detail, "cp ") || strings.Contains(detail, "person_member ") || strings.Contains(detail, "person_roster ") || strings.Contains(detail, "person_group ") {
							assert.Contains(detail, "SEARCH")
							roster = true
						}
						t.Logf("native bounded provenance emitted=%d SQLiteplan=%s", edges, detail)
					}
					require.NoError(plan.Err())
					require.NoError(plan.Close())
					assert.True(recipient)
					assert.True(roster)
				}
			}
		})
	}
}
