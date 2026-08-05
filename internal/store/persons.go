package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	ErrPersonNotFound         = errors.New("person not found")
	ErrPersonRevisionConflict = errors.New("person revision conflict")
	ErrPersonBindingConflict  = errors.New("participant clusters belong to different persons")
	ErrPersonReferenced       = errors.New("person is referenced by another profile")
)

// PersonBindingConflictError reports the curated people that would be
// combined by a participant link, merge, or promotion. Callers can use
// errors.Is(err, ErrPersonBindingConflict) or errors.As for the person IDs.
type PersonBindingConflictError struct {
	PersonIDs []int64
}

func (e *PersonBindingConflictError) Error() string {
	return fmt.Sprintf("%s: person ids %v", ErrPersonBindingConflict, e.PersonIDs)
}

func (e *PersonBindingConflictError) Unwrap() error {
	return ErrPersonBindingConflict
}

// Person is a durable profile over observed participant identity clusters.
// Bindings remain attached to individual participants: unlinking an identity
// edge never deletes or moves them, so one person may intentionally span
// multiple clusters until the user re-links or unbinds those participants.
type Person struct {
	ID             int64     `json:"id"`
	VCardUID       string    `json:"vcard_uid"`
	DisplayName    *string   `json:"display_name,omitempty"`
	Revision       int64     `json:"revision"`
	ParticipantIDs []int64   `json:"participant_ids"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (s *Store) CreatePersonFromParticipant(participantID int64) (*Person, bool, error) {
	return s.CreatePersonFromParticipantContext(context.Background(), participantID)
}

// CreatePersonFromParticipantContext promotes the participant's current
// identity cluster. Promotion is idempotent when the cluster is already
// represented by one person, and fills any unbound members into that person.
// The returned bool reports whether a new person row was created, so callers
// can distinguish creation from an idempotent re-promotion.
func (s *Store) CreatePersonFromParticipantContext(
	ctx context.Context, participantID int64,
) (*Person, bool, error) {
	if participantID <= 0 {
		return nil, false, fmt.Errorf("promote participant %d: %w", participantID, ErrInvalidParticipantID)
	}

	var personID int64
	var person *Person
	var created bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM participants WHERE id = ?`, participantID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("verify participant %d: %w", participantID, err)
		}
		if exists == 0 {
			return fmt.Errorf("participant %d: %w", participantID, ErrParticipantNotFound)
		}

		edges, err := s.loadLinkEdgesTx(tx)
		if err != nil {
			return err
		}
		members := sortedComponentMembers(participantID, edges)
		personIDs, err := personIDsForParticipantsTx(ctx, tx, members)
		if err != nil {
			return err
		}
		if len(personIDs) > 1 {
			return newPersonBindingConflict(personIDs)
		}
		created = len(personIDs) == 0
		if created {
			uid, err := newVCardUID()
			if err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx,
				`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`, uid,
			).Scan(&personID); err != nil {
				return fmt.Errorf("create person: %w", err)
			}
		} else {
			personID = personIDs[0]
		}
		bindingsChanged, err := s.bindPersonParticipantsTx(ctx, tx, personID, members)
		if err != nil {
			return err
		}
		if !created && bindingsChanged {
			if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
				return err
			}
		}
		person, err = s.getPersonTx(ctx, tx, personID)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return person, created, nil
}

// bindPersonParticipantsTx binds every member to the person, ignoring
// members already bound to it, and reports whether any binding was added.
// Callers must have verified that no member is bound to a different person.
func (s *Store) bindPersonParticipantsTx(
	ctx context.Context, tx *loggedTx, personID int64, members []int64,
) (bool, error) {
	insert := s.dialect.InsertOrIgnore(
		`INSERT OR IGNORE INTO person_participants (person_id, participant_id) VALUES (?, ?)`,
	)
	bindingsChanged := false
	for _, memberID := range members {
		result, err := tx.ExecContext(ctx, insert, personID, memberID)
		if err != nil {
			return false, fmt.Errorf("bind person %d to participant %d: %w", personID, memberID, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("check person %d binding for participant %d: %w", personID, memberID, err)
		}
		bindingsChanged = bindingsChanged || changed > 0
	}
	return bindingsChanged, nil
}

func (s *Store) DeletePerson(id, expectedRevision int64) error {
	return s.DeletePersonContext(context.Background(), id, expectedRevision)
}

// DeletePersonContext permanently deletes a person and its participant
// bindings under revision compare-and-swap. Deletion retires the person's
// vCard UID forever: UIDs are random and never reused, so re-promoting the
// same cluster afterwards creates a new person with a new UID. Returns
// ErrPersonNotFound if no such person exists and ErrPersonRevisionConflict
// if expectedRevision is stale.
func (s *Store) DeletePersonContext(ctx context.Context, id, expectedRevision int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var revision int64
		err := tx.QueryRowContext(ctx,
			`SELECT revision FROM persons WHERE id = ?`+s.dialect.SelectForUpdate(),
			id).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		}
		if err != nil {
			return fmt.Errorf("verify person %d before delete: %w", id, err)
		}
		if revision != expectedRevision {
			return ErrPersonRevisionConflict
		}
		var references int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM person_attribute_values
			WHERE value_record_type = 'person' AND value_record_id = ?
			  AND person_id <> ?
			  AND active_until IS NULL AND superseded_at IS NULL
		`, id, id).Scan(&references); err != nil {
			return fmt.Errorf("check references to person %d: %w", id, err)
		}
		if references > 0 {
			return fmt.Errorf("delete person %d: %w", id, ErrPersonReferenced)
		}
		var deletedID int64
		err = tx.QueryRowContext(ctx,
			`DELETE FROM persons WHERE id = ? AND revision = ? RETURNING id`,
			id, expectedRevision).Scan(&deletedID)
		if errors.Is(err, sql.ErrNoRows) {
			return s.personCASMissTx(ctx, tx, id)
		}
		if err != nil {
			return fmt.Errorf("delete person %d: %w", id, err)
		}
		return nil
	})
}

func (s *Store) GetPerson(id int64) (*Person, error) {
	return s.GetPersonContext(context.Background(), id)
}

func (s *Store) GetPersonContext(ctx context.Context, id int64) (*Person, error) {
	var person *Person
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		person, err = s.getPersonTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return person, nil
}

func (s *Store) ListPersons() ([]Person, error) {
	return s.ListPersonsContext(context.Background())
}

func (s *Store) ListPersonsContext(ctx context.Context) ([]Person, error) {
	var persons []Person
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		persons, err = s.listPersonsTx(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return persons, nil
}

func (s *Store) UpdatePersonDisplayName(
	id, expectedRevision int64, displayName *string,
) (*Person, error) {
	return s.UpdatePersonDisplayNameContext(context.Background(), id, expectedRevision, displayName)
}

func (s *Store) UpdatePersonDisplayNameContext(
	ctx context.Context, id, expectedRevision int64, displayName *string,
) (*Person, error) {
	displayName = normalizePersonDisplayName(displayName)
	var person *Person
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var updatedID int64
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`
			UPDATE persons
			SET display_name = ?, revision = revision + 1, updated_at = %s
			WHERE id = ? AND revision = ?
			RETURNING id
		`, s.dialect.Now()), displayName, id, expectedRevision).Scan(&updatedID)
		if errors.Is(err, sql.ErrNoRows) {
			return s.personCASMissTx(ctx, tx, id)
		}
		if err != nil {
			return fmt.Errorf("update person %d: %w", id, err)
		}
		person, err = s.getPersonTx(ctx, tx, updatedID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return person, nil
}

func (s *Store) personCASMissTx(ctx context.Context, tx *loggedTx, id int64) error {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPersonNotFound
	}
	if err != nil {
		return fmt.Errorf("check person %d after revision miss: %w", id, err)
	}
	return ErrPersonRevisionConflict
}

func (s *Store) getPersonTx(ctx context.Context, tx *loggedTx, id int64) (*Person, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT p.id, p.vcard_uid, p.display_name, p.revision, p.created_at, p.updated_at,
		       pp.participant_id
		FROM persons p
		LEFT JOIN person_participants pp ON pp.person_id = p.id
		WHERE p.id = ?
		ORDER BY pp.participant_id
	`, id)
	if err != nil {
		return nil, fmt.Errorf("get person %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var person *Person
	for rows.Next() {
		rowPerson, participantID, err := scanPersonBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person %d: %w", id, err)
		}
		if person == nil {
			person = rowPerson
			person.ParticipantIDs = []int64{}
		}
		if participantID.Valid {
			person.ParticipantIDs = append(person.ParticipantIDs, participantID.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person %d: %w", id, err)
	}
	if person == nil {
		return nil, ErrPersonNotFound
	}
	return person, nil
}

func (s *Store) listPersonsTx(ctx context.Context, tx *loggedTx) ([]Person, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT p.id, p.vcard_uid, p.display_name, p.revision, p.created_at, p.updated_at,
		       pp.participant_id
		FROM persons p
		LEFT JOIN person_participants pp ON pp.person_id = p.id
		ORDER BY CASE WHEN p.display_name IS NULL THEN 1 ELSE 0 END,
		         LOWER(p.display_name), p.id, pp.participant_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list persons: %w", err)
	}
	defer func() { _ = rows.Close() }()

	persons := make([]Person, 0)
	for rows.Next() {
		person, participantID, err := scanPersonBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person: %w", err)
		}
		if len(persons) == 0 || persons[len(persons)-1].ID != person.ID {
			person.ParticipantIDs = []int64{}
			persons = append(persons, *person)
		}
		if participantID.Valid {
			last := len(persons) - 1
			persons[last].ParticipantIDs = append(persons[last].ParticipantIDs, participantID.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate persons: %w", err)
	}
	return persons, nil
}

func personIDsForParticipantsTx(ctx context.Context, tx *loggedTx, participantIDs []int64) ([]int64, error) {
	if len(participantIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(participantIDs)), ",")
	args := make([]any, len(participantIDs))
	for i, id := range participantIDs {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT person_id FROM person_participants
		WHERE participant_id IN (%s)
		GROUP BY person_id
		ORDER BY person_id
	`, placeholders), args...)
	if err != nil {
		return nil, fmt.Errorf("look up participant person bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	personIDs := make([]int64, 0, 2)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan participant person binding: %w", err)
		}
		personIDs = append(personIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate participant person bindings: %w", err)
	}
	return personIDs, nil
}

// personForClusterUnionTx resolves the single person covering the union of
// a's and b's clusters: (0, members, nil) when no member is bound, the
// person's ID when exactly one person covers the union, and a
// PersonBindingConflictError when the union spans two curated people.
// Link and merge use it both to refuse combining different persons and to
// fill the covering person's bindings across the just-combined cluster.
func (s *Store) personForClusterUnionTx(
	ctx context.Context, tx *loggedTx, a, b int64, edges []linkEdge,
) (int64, []int64, error) {
	members := componentOf(a, edges)
	for id := range componentOf(b, edges) {
		members[id] = struct{}{}
	}
	ids := make([]int64, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	personIDs, err := personIDsForParticipantsTx(ctx, tx, ids)
	if err != nil {
		return 0, nil, err
	}
	if len(personIDs) > 1 {
		return 0, nil, newPersonBindingConflict(personIDs)
	}
	if len(personIDs) == 0 {
		return 0, ids, nil
	}
	return personIDs[0], ids, nil
}

func (s *Store) PersonForParticipants(participantIDs []int64) (*Person, error) {
	return s.PersonForParticipantsContext(context.Background(), participantIDs)
}

// PersonForParticipantsContext returns the single person bound to any of the
// given participants, or nil when none is. Participants of one identity
// cluster are bound all-or-none to at most one person (link, merge, and
// promotion all enforce this), so callers pass a cluster's member IDs to
// resolve the cluster's curated person. Returns a PersonBindingConflictError
// if the IDs unexpectedly span two persons.
func (s *Store) PersonForParticipantsContext(
	ctx context.Context, participantIDs []int64,
) (*Person, error) {
	var person *Person
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		personIDs, err := personIDsForParticipantsTx(ctx, tx, participantIDs)
		if err != nil {
			return err
		}
		if len(personIDs) > 1 {
			return newPersonBindingConflict(personIDs)
		}
		if len(personIDs) == 0 {
			return nil
		}
		person, err = s.getPersonTx(ctx, tx, personIDs[0])
		return err
	})
	if err != nil {
		return nil, err
	}
	return person, nil
}

// mergePersonBindingsTx carries the covering person's bindings through a
// participant merge: the absorbed participant's binding is dropped and every
// surviving member of the combined cluster is bound, so the person exactly
// covers the merged cluster. Reports whether any binding changed. Callers
// must have resolved personID via personForClusterUnionTx, which rejects
// merges that would combine two different persons.
func (s *Store) mergePersonBindingsTx(
	ctx context.Context, tx *loggedTx, personID, absorbed int64, members []int64,
) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`DELETE FROM person_participants WHERE participant_id = ?`, absorbed)
	if err != nil {
		return false, fmt.Errorf("drop absorbed person binding for participant %d: %w", absorbed, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check absorbed person binding for participant %d: %w", absorbed, err)
	}
	survivors := make([]int64, 0, len(members))
	for _, memberID := range members {
		if memberID != absorbed {
			survivors = append(survivors, memberID)
		}
	}
	filled, err := s.bindPersonParticipantsTx(ctx, tx, personID, survivors)
	if err != nil {
		return false, err
	}
	return removed > 0 || filled, nil
}

func (s *Store) bumpPersonRevisionsTx(ctx context.Context, tx *loggedTx, personIDs ...int64) error {
	slices.Sort(personIDs)
	personIDs = slices.Compact(personIDs)
	if len(personIDs) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(personIDs)), ",")
	args := make([]any, len(personIDs))
	for i, id := range personIDs {
		args[i] = id
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE persons
		SET revision = revision + 1, updated_at = %s
		WHERE id IN (%s)
	`, s.dialect.Now(), placeholders), args...); err != nil {
		return fmt.Errorf("bump person revisions: %w", err)
	}
	return nil
}

func sortedComponentMembers(id int64, edges []linkEdge) []int64 {
	component := componentOf(id, edges)
	members := make([]int64, 0, len(component))
	for memberID := range component {
		members = append(members, memberID)
	}
	slices.Sort(members)
	return members
}

func newPersonBindingConflict(ids []int64) error {
	ids = append([]int64(nil), ids...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	return &PersonBindingConflictError{PersonIDs: ids}
}

func normalizePersonDisplayName(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func newVCardUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate person vCard UID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func scanPersonBinding(row scanner) (*Person, sql.NullInt64, error) {
	var person Person
	var displayName sql.NullString
	var participantID sql.NullInt64
	if err := row.Scan(
		&person.ID, &person.VCardUID, &displayName, &person.Revision,
		&person.CreatedAt, &person.UpdatedAt, &participantID,
	); err != nil {
		return nil, sql.NullInt64{}, err
	}
	if displayName.Valid {
		person.DisplayName = &displayName.String
	}
	return &person, participantID, nil
}
