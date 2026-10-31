package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// These accepted combinations are independent of the SQL and Go predicates.
// Excluded lists and non-direct evidence never count.
func TestCountedEventWhereMatchesTierRule(t *testing.T) {
	accepted := map[string]bool{
		"attendee/inbound/meeting/0": true, "attendee/inbound/meeting/1": true,
		"attendee/outbound/meeting/0": true, "attendee/outbound/meeting/1": true,
		"organizer/inbound/meeting/0": true, "organizer/inbound/meeting/1": true,
		"organizer/outbound/meeting/0": true, "organizer/outbound/meeting/1": true,
		"sender/inbound/message/0":     true,
		"addressed/outbound/message/0": true, "addressed/outbound/message/1": true,
		"member/outbound/message/0": true, "member/outbound/message/1": true,
	}
	rows := countedEventRows(t)
	for _, row := range rows {
		key := row.role + "/" + row.direction + "/" + row.refKind + "/0"
		if row.large {
			key = key[:len(key)-1] + "1"
		}
		want := row.evidence == "direct" && !row.list && accepted[key]
		assert.Equal(t, want, row.counted, "%+v", row)
	}
}

func TestCountedEventGoMatchesSQL(t *testing.T) {
	for _, row := range countedEventRows(t) {
		got := store.CountedEvent(store.ActivityEvidence(row.evidence), store.ActivityRole(row.role), store.ActivityDirection(row.direction), row.refKind, row.list, row.large)
		assert.Equal(t, row.counted, got, "%+v", row)
	}
}

type countedEventRow struct {
	evidence, role, direction, refKind string
	list, large, counted               bool
}

func countedEventRows(t *testing.T) []countedEventRow {
	t.Helper()
	st := testutil.NewTestStore(t)
	predicate := store.CountedEventWhere(store.CountedEventInputs{
		Evidence: "e.value", Role: "r.value", Direction: "d.value", RefKind: "k.value",
		ListExcluded: "l.value", LargeInbound: "a.value",
	})
	rows, err := st.DB().QueryContext(t.Context(), `WITH
 evidence(value) AS (VALUES ('direct'),('co_presence'),('unknown')),
 roles(value) AS (VALUES ('sender'),('addressed'),('member'),('attendee'),('organizer'),('unknown')),
 directions(value) AS (VALUES ('inbound'),('outbound'),('observed'),('unknown')),
 kinds(value) AS (VALUES ('message'),('meeting'),('call'),('unknown')),
 lists(value) AS (VALUES (0),(1)), audiences(value) AS (VALUES (0),(1))
 SELECT e.value,r.value,d.value,k.value,l.value,a.value,CASE WHEN `+predicate+` THEN 1 ELSE 0 END
 FROM evidence e CROSS JOIN roles r CROSS JOIN directions d CROSS JOIN kinds k CROSS JOIN lists l CROSS JOIN audiences a`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []countedEventRow
	for rows.Next() {
		var row countedEventRow
		require.NoError(t, rows.Scan(&row.evidence, &row.role, &row.direction, &row.refKind, &row.list, &row.large, &row.counted))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())
	require.Len(t, result, 1152)
	return result
}
