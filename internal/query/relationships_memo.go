package query

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// relationshipsMemoMaxEntries bounds the memo to a handful of distinct
// (revision, filters, show_all, decay-date) result sets. The hub landing page
// always reuses one key; a few more cover facet variations without letting a
// scripted filter sweep grow the cache without bound.
const relationshipsMemoMaxEntries = 4

// relationshipsMemo caches fully ranked relationship candidate lists per
// memo key (see relationshipsMemoKey). Staleness is structurally impossible:
// the committed cache revision — which folds the identity revision — is part
// of every key, so a revision bump stops hitting old entries, and they age
// out FIFO. Concurrent identical requests share one computation via
// singleflight; errors are never cached.
//
// Cached slices are shared across callers and must be treated as immutable.
type relationshipsMemo struct {
	group   singleflight.Group
	mu      sync.Mutex
	entries map[string][]RelationshipRow
	order   []string
}

// relationshipsMemoKey identifies one cacheable ranked candidate list.
// Pagination (limit/offset) is deliberately absent: pages are slices of one
// cached list. Now contributes only its UTC date because decay is computed
// with date_diff('day', occurred_at, now) against a naive-UTC timestamp, so
// every instant within one UTC day yields identical results.
func relationshipsMemoKey(revision, conditions string, args []any, showAll bool, now time.Time) string {
	var key strings.Builder
	fmt.Fprintf(&key, "%s|%t|%s|%s", revision, showAll, now.UTC().Format(time.DateOnly), conditions)
	for _, arg := range args {
		fmt.Fprintf(&key, "\x1f%v", arg)
	}
	return key.String()
}

// rows returns the cached candidate list for key, computing and storing it on
// a miss. compute runs at most once per in-flight key; its error is returned
// to every concurrent caller and not cached.
func (m *relationshipsMemo) rows(key string, compute func() ([]RelationshipRow, error)) ([]RelationshipRow, error) {
	if rows, ok := m.get(key); ok {
		return rows, nil
	}
	value, err, _ := m.group.Do(key, func() (any, error) {
		if rows, ok := m.get(key); ok {
			return rows, nil
		}
		rows, err := compute()
		if err != nil {
			return nil, err
		}
		m.put(key, rows)
		return rows, nil
	})
	if err != nil {
		// The only error singleflight can surface here is compute's own,
		// which is already wrapped with query context at its source.
		return nil, err //nolint:wrapcheck
	}
	rows, ok := value.([]RelationshipRow)
	if !ok {
		return nil, fmt.Errorf("relationships memo returned unexpected type %T", value)
	}
	return rows, nil
}

func (m *relationshipsMemo) get(key string) ([]RelationshipRow, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, ok := m.entries[key]
	return rows, ok
}

func (m *relationshipsMemo) put(key string, rows []RelationshipRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[string][]RelationshipRow, relationshipsMemoMaxEntries)
	}
	if _, exists := m.entries[key]; !exists {
		m.order = append(m.order, key)
	}
	m.entries[key] = rows
	for len(m.order) > relationshipsMemoMaxEntries {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.entries, oldest)
	}
}
