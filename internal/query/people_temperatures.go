package query

import (
	"context"
	"errors"
	"fmt"
	"go.kenn.io/msgvault/internal/identityindex"
	"slices"
	"strings"
)

// PeopleTemperatures reads a published cache once and never starts a build.
// Each durable person receives the highest temperature among bound participants.
func (e *DuckDBEngine) PeopleTemperatures(ctx context.Context, members map[int64][]int64) (map[int64]int, bool, error) {
	out := map[int64]int{}
	if e.analyticsDir == "" {
		return out, false, nil
	}
	if len(members) > 500 {
		return nil, false, errors.New("temperature read: at most 500 people")
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return out, false, err
	}
	defer release()
	if _, err := ReadCacheSyncState(e.analyticsDir); err != nil {
		return out, false, fmt.Errorf("read published cache state: %w", err)
	}
	values := []string{}
	args := []any{}
	ids := make([]int64, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		for _, participant := range members[id] {
			values = append(values, "(CAST(? AS BIGINT),CAST(? AS BIGINT))")
			args = append(args, id, participant)
		}
	}
	if len(values) == 0 {
		return out, true, nil
	}
	path := quoteIdentitySQLPath(e.parquetPath(identityindex.DatasetPeople))
	rows, err := e.db.QueryContext(ctx, `WITH candidates(person_id,participant_id) AS (VALUES `+strings.Join(values, ",")+`) SELECT c.person_id,MAX(d.current_temperature) FROM candidates c JOIN read_parquet('`+path+`') d ON list_contains(d.member_ids,c.participant_id) GROUP BY c.person_id`, args...)
	if err != nil {
		return out, false, fmt.Errorf("read people temperatures: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var temperature int
		if err := rows.Scan(&id, &temperature); err != nil {
			return out, false, fmt.Errorf("scan people temperature: %w", err)
		}
		out[id] = temperature
	}
	if err := rows.Err(); err != nil {
		return out, false, fmt.Errorf("iterate people temperatures: %w", err)
	}
	return out, true, nil
}
