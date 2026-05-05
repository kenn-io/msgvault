package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Collection is a named grouping of sources that should be treated as
// a single logical archive.
type Collection struct {
	ID          int64
	Name        string
	Description string
	CreatedAt   time.Time
}

// CollectionWithSources bundles a Collection with its member source
// IDs and a message-count aggregate.
type CollectionWithSources struct {
	Collection
	SourceIDs    []int64
	MessageCount int64
}

// DefaultCollectionName is the always-present collection that mirrors
// every source. It is auto-managed by EnsureDefaultCollection on every
// schema init, so explicit add/remove/delete operations against it are
// rejected; the next CLI invocation would silently revert any change.
const DefaultCollectionName = "All"

// ErrCollectionNotFound is returned when a collection lookup has no hits.
var ErrCollectionNotFound = errors.New("collection not found")

// ErrCollectionImmutable is returned when an explicit mutation is
// attempted against the auto-managed default collection.
var ErrCollectionImmutable = errors.New(
	`cannot modify the auto-managed "All" collection`,
)

// EnsureDefaultCollection creates the auto-managed default collection
// if it doesn't exist and adds all current sources to it. Safe to call
// on every schema init. Mutations to this collection are rejected by
// AddSourcesToCollection / RemoveSourcesFromCollection / DeleteCollection
// so users don't get a silent revert on the next CLI invocation.
func (s *Store) EnsureDefaultCollection() error {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM collections WHERE name = ?`, DefaultCollectionName,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := s.db.Exec(
			`INSERT INTO collections (name, description)
			 VALUES (?, 'All accounts')`,
			DefaultCollectionName,
		)
		if err != nil {
			return fmt.Errorf("create default collection: %w", err)
		}
		id, _ = res.LastInsertId()
	} else if err != nil {
		return fmt.Errorf("check default collection: %w", err)
	}

	// Add all sources not already in it.
	_, err = s.db.Exec(
		`INSERT OR IGNORE INTO collection_sources (collection_id, source_id)
		 SELECT ?, id FROM sources`,
		id,
	)
	return err
}

// CreateCollection inserts a new collection with the given name,
// description, and member source IDs.
func (s *Store) CreateCollection(
	name, description string, sourceIDs []int64,
) (*Collection, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("collection name is required")
	}
	if name == DefaultCollectionName {
		// Mirror the AddSourcesToCollection / RemoveSourcesFromCollection
		// / DeleteCollection guards: the default collection is auto-
		// managed by EnsureDefaultCollection. A manual create of "All"
		// would have raced the auto-create; rejecting up front gives
		// the consistent error surface as the rest of the collection
		// surface.
		return nil, ErrCollectionImmutable
	}
	if len(sourceIDs) == 0 {
		return nil, fmt.Errorf(
			"collection %q needs at least one source", name,
		)
	}

	unique := uniqueInt64s(sourceIDs)
	if err := s.validateSourceIDs(unique); err != nil {
		return nil, err
	}

	var created *Collection
	err := s.withTx(func(tx *loggedTx) error {
		res, err := tx.Exec(
			`INSERT INTO collections (name, description)
			 VALUES (?, ?)`,
			name, description,
		)
		if err != nil {
			if isSQLiteError(err, "UNIQUE constraint failed") {
				return fmt.Errorf(
					"collection %q already exists", name,
				)
			}
			return fmt.Errorf("insert collection: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("last insert id: %w", err)
		}

		for _, sid := range unique {
			if _, err := tx.Exec(
				`INSERT INTO collection_sources
				  (collection_id, source_id)
				 VALUES (?, ?)`,
				id, sid,
			); err != nil {
				return fmt.Errorf("link source %d: %w", sid, err)
			}
		}

		row := tx.QueryRow(
			`SELECT id, name, description, created_at
			 FROM collections WHERE id = ?`, id,
		)
		c, scanErr := scanCollection(row)
		if scanErr != nil {
			return scanErr
		}
		created = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// GetCollectionByName returns the collection with the given name and
// its member source IDs.
func (s *Store) GetCollectionByName(
	name string,
) (*CollectionWithSources, error) {
	row := s.db.QueryRow(
		`SELECT id, name, description, created_at
		 FROM collections WHERE name = ?`, name,
	)
	c, err := scanCollection(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCollectionNotFound
		}
		return nil, err
	}
	return s.hydrateCollection(c)
}

// ListCollections returns every collection with source IDs and
// message counts.
func (s *Store) ListCollections() ([]*CollectionWithSources, error) {
	rows, err := s.db.Query(
		`SELECT id, name, description, created_at
		 FROM collections ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var collections []*Collection
	for rows.Next() {
		c, scanErr := scanCollection(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		collections = append(collections, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]*CollectionWithSources, 0, len(collections))
	for _, c := range collections {
		hydrated, err := s.hydrateCollection(c)
		if err != nil {
			return nil, err
		}
		result = append(result, hydrated)
	}
	return result, nil
}

// getCollectionID looks up a collection ID by name without hydrating.
func (s *Store) getCollectionID(name string) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM collections WHERE name = ?`, name,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrCollectionNotFound
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AddSourcesToCollection attaches sources to a collection. Idempotent.
// Rejects mutations of the auto-managed default collection.
func (s *Store) AddSourcesToCollection(name string, sourceIDs []int64) error {
	if name == DefaultCollectionName {
		return ErrCollectionImmutable
	}
	if err := s.validateSourceIDs(sourceIDs); err != nil {
		return err
	}
	collID, err := s.getCollectionID(name)
	if err != nil {
		return err
	}
	return s.withTx(func(tx *loggedTx) error {
		for _, sid := range sourceIDs {
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO collection_sources
				  (collection_id, source_id)
				 VALUES (?, ?)`,
				collID, sid,
			); err != nil {
				return fmt.Errorf("add source %d: %w", sid, err)
			}
		}
		return nil
	})
}

// RemoveSourcesFromCollection detaches sources. Idempotent.
// Rejects mutations of the auto-managed default collection.
func (s *Store) RemoveSourcesFromCollection(name string, sourceIDs []int64) error {
	if name == DefaultCollectionName {
		return ErrCollectionImmutable
	}
	if err := s.validateSourceIDs(sourceIDs); err != nil {
		return err
	}
	collID, err := s.getCollectionID(name)
	if err != nil {
		return err
	}
	return s.withTx(func(tx *loggedTx) error {
		for _, sid := range sourceIDs {
			if _, err := tx.Exec(
				`DELETE FROM collection_sources
				 WHERE collection_id = ? AND source_id = ?`,
				collID, sid,
			); err != nil {
				return fmt.Errorf("remove source %d: %w", sid, err)
			}
		}
		return nil
	})
}

// DeleteCollection drops the collection. Sources and messages untouched.
// Rejects deletion of the auto-managed default collection.
func (s *Store) DeleteCollection(name string) error {
	if name == DefaultCollectionName {
		return ErrCollectionImmutable
	}
	res, err := s.db.Exec(
		`DELETE FROM collections WHERE name = ?`, name,
	)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrCollectionNotFound
	}
	return nil
}

func (s *Store) hydrateCollection(
	c *Collection,
) (*CollectionWithSources, error) {
	rows, err := s.db.Query(
		`SELECT source_id FROM collection_sources
		 WHERE collection_id = ?
		 ORDER BY source_id`,
		c.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("load sources for %s: %w", c.Name, err)
	}
	var sourceIDs []int64
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		sourceIDs = append(sourceIDs, sid)
	}
	// Idiomatic ordering: check rows.Err() before closing so the
	// iteration error is observed against an open rows handle.
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	var count int64
	if len(sourceIDs) > 0 {
		count, err = s.CountActiveMessages(sourceIDs...)
		if err != nil {
			return nil, err
		}
	}

	return &CollectionWithSources{
		Collection:   *c,
		SourceIDs:    sourceIDs,
		MessageCount: count,
	}, nil
}

func scanCollection(row interface {
	Scan(dest ...any) error
}) (*Collection, error) {
	var c Collection
	if err := row.Scan(
		&c.ID, &c.Name, &c.Description, &c.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) validateSourceIDs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := "SELECT id FROM sources WHERE id IN (" +
		strings.Join(placeholders, ",") + ")"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("validate source IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	found := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if !found[id] {
			return fmt.Errorf("source %d not found", id)
		}
	}
	return nil
}

func uniqueInt64s(in []int64) []int64 {
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
