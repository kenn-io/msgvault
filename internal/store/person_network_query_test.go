package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonNetworkIDValuesCTECastsEveryParameterOnlyIDRowAsBigint(t *testing.T) {
	tests := []struct {
		name    string
		cteName string
		want    string
	}{
		{
			name:    "person frontier",
			cteName: "frontier_people",
			want:    "frontier_people(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
		{
			name:    "organization frontier",
			cteName: "frontier_organizations",
			want:    "frontier_organizations(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
		{
			name:    "seen relationship edges",
			cteName: "admitted_relationships",
			want:    "admitted_relationships(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
		{
			name:    "seen employment edges",
			cteName: "admitted_employments",
			want:    "admitted_employments(id) AS (VALUES (CAST(? AS BIGINT)), (CAST(? AS BIGINT)))",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query, args := personNetworkIDValuesCTE(test.cteName, []int64{9, 3})

			assert.Equal(t, test.want, query)
			assert.Equal(t, []any{int64(3), int64(9)}, args)
		})
	}
}

// This catches a layer query that scans person_relationships or employments
// instead of probing the adjacency indexes for each frontier node. The
// public-order sort over the candidates is expected; a table scan is not.
func TestPersonNetworkLayerQueryProbesAdjacencyIndexes(t *testing.T) {
	st, err := OpenForTest(filepath.Join(t.TempDir(), "network-query-plan.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())

	tests := []struct {
		name          string
		people        []int64
		organizations []int64
		seen          map[string][]int64
		includeEnded  bool
		wantProbes    []string
	}{
		{
			name:   "current person edges",
			people: []int64{7},
			wantProbes: []string{
				"SEARCH relationship USING INDEX idx_person_relationships_source_current_edge (source_person_id=?)",
				"SEARCH relationship USING INDEX idx_person_relationships_target_current_edge (target_person_id=?)",
				"SEARCH employment USING COVERING INDEX idx_employments_active_person_org_title (person_id=?)",
			},
		},
		{
			name:          "current organization employments excluding seen edges",
			organizations: []int64{9},
			seen:          map[string][]int64{"employment": {1, 2}},
			wantProbes:    []string{"SEARCH employment USING INDEX idx_employments_organization (organization_id=?)"},
		},
		{
			name:          "all organization employments",
			organizations: []int64{9},
			includeEnded:  true,
			wantProbes:    []string{"SEARCH employment USING INDEX idx_employments_organization (organization_id=?)"},
		},
		{
			name:          "mixed frontier excluding seen edges",
			people:        []int64{7, 11},
			organizations: []int64{9},
			seen:          map[string][]int64{"relationship": {3}, "employment": {1}},
			wantProbes: []string{
				"SEARCH relationship USING INDEX idx_person_relationships_source_current_edge (source_person_id=?)",
				"SEARCH relationship USING INDEX idx_person_relationships_target_current_edge (target_person_id=?)",
				"SEARCH employment USING COVERING INDEX idx_employments_active_person_org_title (person_id=?)",
				"SEARCH employment USING INDEX idx_employments_organization_current_edge (organization_id=?)",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			query, args := st.personNetworkLayerSourcesQuery(
				test.people, test.organizations, test.seen, test.includeEnded, 501)
			rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			details := make([]string, 0)
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(rows.Err())
			plan := strings.Join(details, "\n")
			assert.NotContains(plan, "SCAN person_relationships")
			assert.NotContains(plan, "SCAN employments")
			for _, probe := range test.wantProbes {
				assert.Contains(plan, probe)
			}
		})
	}
}
