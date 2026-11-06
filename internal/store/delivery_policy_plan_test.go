package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveryPolicyAuditLookupUsesIndexSQLite(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "delivery-policy-plan.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())

	rows, err := st.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+st.deliveryApprovalCoversPointsQuery(),
		"example-person", DeliveryPersonAllRoutes, 1)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(rows.Err())
	plan := strings.Join(details, "\n")
	assert.Contains(plan, "SEARCH delivery_policy_audit USING INDEX idx_delivery_policy_audit_latest")
	assert.NotContains(plan, "USE TEMP B-TREE")
}
