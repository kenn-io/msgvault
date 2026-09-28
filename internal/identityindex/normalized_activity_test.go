package identityindex

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandedActivityFiltersReachParquet(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, root, syntheticRelationshipFanOutOptions{
		firstMessageID: 1, messageCount: 20_000, memberCount: 100,
		startDate: "2023-12-31 22:00:00", messageType: "whatsapp", conversationType: "group_chat",
	})
	path := func(dataset string) string { return parquetDatasetGlob(root, dataset) }
	writeRelationshipParquet(t, db, root, DatasetActivity, "("+buildSparseRelationshipActivitySQL(path, readParquetRelation([]string{path("messages")}, true), 2023)+") UNION ALL ("+buildSparseRelationshipActivitySQL(path, readParquetRelation([]string{path("messages")}, true), 2024)+")")
	relation := ExpandedActivityRelation(
		readParquetRelation([]string{filepath.Join(root, DatasetActivity, "*.parquet")}, false),
		readParquetRelation([]string{path("conversation_participants")}, false),
		readParquetRelation([]string{path("participants")}, false),
		readParquetRelation([]string{path("participant_clusters")}, false),
		readParquetRelation([]string{path("owner_participants")}, false),
	)
	for _, tc := range []struct {
		predicate string
		filter    string
		want      int64
	}{
		{"occurred_year = 2024", "occurred_year=2024", 1_280_000},
		{"message_id = 7", "message_id=7", 100},
		{"occurred_year = 2024 AND canonical_id = 5", "occurred_year=2024", 12_800},
	} {
		t.Run(tc.predicate, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var kind, plan string
			requirements.NoError(db.QueryRow("EXPLAIN (FORMAT JSON) SELECT * FROM "+relation+" WHERE "+tc.predicate).Scan(&kind, &plan))
			type node struct {
				Name      string         `json:"name"`
				ExtraInfo map[string]any `json:"extra_info"`
				Children  []node         `json:"children"`
			}
			var nodes []node
			requirements.NoError(json.Unmarshal([]byte(plan), &nodes))
			// Inspect scans carrying message payload. Membership and existence
			// joins may separately read narrow key columns. With a person filter,
			// the orphan branch disappears and all activity scans should be scoped.
			scans := 0
			var visit func([]node)
			visit = func(nodes []node) {
				for _, n := range nodes {
					projections := fmt.Sprint(n.ExtraInfo["Projections"])
					if strings.Contains(n.Name, "PARQUET") &&
						strings.Contains(projections, "message_id") &&
						(strings.Contains(projections, "source_type") || strings.Contains(tc.predicate, "canonical_id")) {
						scans++
						assertions.Contains(fmt.Sprint(n.ExtraInfo["Filters"]), tc.filter)
					}
					visit(n.Children)
				}
			}
			visit(nodes)
			assertions.Positive(scans)
			var count int64
			requirements.NoError(db.QueryRow("SELECT count(*) FROM " + relation + " WHERE " + tc.predicate).Scan(&count))
			assertions.Equal(tc.want, count)
		})
	}
}
