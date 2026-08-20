package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	ErrEmploymentNotFound         = errors.New("employment not found")
	ErrEmploymentRevisionConflict = errors.New("employment revision conflict")
	ErrEmploymentInvalid          = errors.New("invalid employment")
	ErrEmploymentPrimaryConflict  = errors.New("person already has a primary current employment")
	ErrEmploymentDuplicateActive  = errors.New("person already has a current employment with this organization and title")
)

type Employment struct {
	ID             int64        `json:"id"`
	PersonID       int64        `json:"person_id"`
	OrganizationID int64        `json:"organization_id"`
	Title          *string      `json:"title,omitempty"`
	Role           *string      `json:"role,omitempty"`
	Department     *string      `json:"department,omitempty"`
	Location       *string      `json:"location,omitempty"`
	AddressID      *int64       `json:"address_id,omitempty"`
	Description    *string      `json:"description,omitempty"`
	StartDate      *PartialDate `json:"start_date,omitempty"`
	EndDate        *PartialDate `json:"end_date,omitempty"`
	IsCurrent      bool         `json:"is_current"`
	IsPrimary      bool         `json:"is_primary"`
	Source         Provenance   `json:"source"`
	SourceRef      *string      `json:"source_ref,omitempty"`
	Confidence     *float64     `json:"confidence,omitempty"`
	Revision       int64        `json:"revision"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

type EmploymentInput struct {
	PersonID, OrganizationID          int64
	Title, Role, Department, Location *string
	AddressID                         *int64
	Description                       *string
	StartDate, EndDate                *PartialDate
	IsCurrent, IsPrimary              *bool
	Source                            Provenance
	SourceRef                         *string
	Confidence                        *float64
}

type EmploymentFilter struct {
	PersonID, OrganizationID int64
	CurrentOnly              bool
	Limit, Offset            int
}

const (
	DefaultEmploymentPageSize = 200
	MaxEmploymentPageSize     = 1000
)

const employmentColumns = `
	id, person_id, organization_id, title, role, department, location, address_id,
	description, start_year, start_month, start_day, end_year, end_month, end_day,
	is_current, is_primary, source, source_ref, confidence, revision, created_at, updated_at`

func (s *Store) AddEmploymentContext(ctx context.Context, input EmploymentInput) (*Employment, error) {
	input, err := validateEmploymentInput(input)
	if err != nil {
		return nil, err
	}
	return retryContendedWrite(ctx, s, "add employment", func() (*Employment, error) {
		var employment *Employment
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.claimEmploymentPeopleTx(ctx, tx, input.PersonID); err != nil {
				return err
			}
			if err := s.verifyEmploymentReferencesTx(ctx, tx, input); err != nil {
				return err
			}
			current := employmentCurrent(input)
			primary, err := s.resolveEmploymentPrimaryTx(ctx, tx, input, current, 0)
			if err != nil {
				return err
			}
			employment, err = s.insertEmploymentTx(ctx, tx, input, current, primary)
			return err
		})
		if err != nil {
			return nil, err
		}
		return employment, nil
	})
}

func (s *Store) GetEmploymentContext(ctx context.Context, id int64) (*Employment, error) {
	employment, err := scanEmployment(s.db.QueryRowContext(ctx, `SELECT `+employmentColumns+` FROM employments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmploymentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get employment %d: %w", id, err)
	}
	return employment, nil
}

func (s *Store) UpdateEmploymentContext(ctx context.Context, id, expectedRevision int64, input EmploymentInput) (*Employment, error) {
	input, err := validateEmploymentInput(input)
	if err != nil {
		return nil, err
	}
	return retryContendedWrite(ctx, s, "update employment", func() (*Employment, error) {
		var employment *Employment
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			snapshot, err := getEmploymentTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if snapshot.Revision != expectedRevision {
				return ErrEmploymentRevisionConflict
			}
			if err := s.claimEmploymentPeopleTx(
				ctx, tx, snapshot.PersonID, input.PersonID); err != nil {
				return err
			}
			if err := s.verifyEmploymentReferencesTx(ctx, tx, input); err != nil {
				return err
			}
			currentEmployment, err := getEmploymentForUpdateTx(ctx, tx, s.dialect, id)
			if err != nil {
				return err
			}
			if currentEmployment.Revision != expectedRevision {
				return ErrEmploymentRevisionConflict
			}
			current := employmentCurrent(input)
			if input.IsCurrent == nil && input.EndDate == nil {
				// Preserve the stored state: an update that mentions neither
				// is_current nor an end date must not resurrect a historical
				// employment.
				current = currentEmployment.IsCurrent
			}
			var primary bool
			if input.IsPrimary == nil {
				// Preserve the stored flag: an update that omits is_primary must
				// not promote as a side effect. Demote when it stops being
				// current, matching EndEmploymentContext.
				primary = currentEmployment.IsPrimary && current
			} else {
				primary, err = s.resolveEmploymentPrimaryTx(ctx, tx, input, current, id)
				if err != nil {
					return err
				}
			}
			args := employmentWriteArgs(input, current, primary)
			args = append(args, id, expectedRevision)
			employment, err = s.employmentWriteWithConflictSavepointTx(
				ctx, tx, input, current, id, func() (*Employment, error) {
					return scanEmployment(tx.QueryRowContext(ctx, fmt.Sprintf(`
						UPDATE employments SET person_id = ?, organization_id = ?, title = ?, title_normalized = ?,
						role = ?, department = ?, location = ?, address_id = ?, description = ?,
						start_year = ?, start_month = ?, start_day = ?, end_year = ?, end_month = ?, end_day = ?,
						is_current = ?, is_primary = ?, source = ?, source_ref = ?, confidence = ?,
						revision = revision + 1, updated_at = %s
						WHERE id = ? AND revision = ? RETURNING %s`, s.dialect.Now(), employmentColumns), args...))
				})
			if errors.Is(err, sql.ErrNoRows) {
				return s.employmentCASMissTx(ctx, tx, id)
			}
			if err != nil {
				if errors.Is(err, ErrEmploymentDuplicateActive) || errors.Is(err, ErrEmploymentPrimaryConflict) {
					return err
				}
				return fmt.Errorf("update employment %d: %w", id, err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return employment, nil
	})
}

func (s *Store) EndEmploymentContext(ctx context.Context, id, expectedRevision int64, endDate PartialDate) (*Employment, error) {
	if err := validateEmploymentDate(endDate, "end"); err != nil {
		return nil, err
	}
	return retryContendedWrite(ctx, s, "end employment", func() (*Employment, error) {
		var employment *Employment
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			snapshot, err := getEmploymentTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if snapshot.Revision != expectedRevision {
				return ErrEmploymentRevisionConflict
			}
			if err := s.claimEmploymentPeopleTx(ctx, tx, snapshot.PersonID); err != nil {
				return err
			}
			current, err := getEmploymentForUpdateTx(ctx, tx, s.dialect, id)
			if err != nil {
				return err
			}
			if current.Revision != expectedRevision {
				return ErrEmploymentRevisionConflict
			}
			if current.StartDate != nil && CompareAtSharedPrecision(endDate, *current.StartDate) < 0 {
				return fmt.Errorf("%w: end date must not precede start date", ErrEmploymentInvalid)
			}
			args := append(PartialDateArgs(endDate), false, false, id, expectedRevision)
			employment, err = scanEmployment(tx.QueryRowContext(ctx, fmt.Sprintf(`
				UPDATE employments SET end_year = ?, end_month = ?, end_day = ?, is_current = ?, is_primary = ?,
				revision = revision + 1, updated_at = %s WHERE id = ? AND revision = ? RETURNING %s`, s.dialect.Now(), employmentColumns), args...))
			if errors.Is(err, sql.ErrNoRows) {
				return s.employmentCASMissTx(ctx, tx, id)
			}
			if err != nil {
				return fmt.Errorf("end employment %d: %w", id, err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return employment, nil
	})
}

func (s *Store) SetPrimaryEmploymentContext(ctx context.Context, id, expectedRevision int64) (*Employment, error) {
	return retryContendedWrite(ctx, s, "set primary employment", func() (*Employment, error) {
		return s.setPrimaryEmploymentOnce(ctx, id, expectedRevision)
	})
}

func (s *Store) setPrimaryEmploymentOnce(ctx context.Context, id, expectedRevision int64) (*Employment, error) {
	var employment *Employment
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		snapshot, err := getEmploymentTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if snapshot.Revision != expectedRevision {
			return ErrEmploymentRevisionConflict
		}
		if err := s.claimEmploymentPeopleTx(ctx, tx, snapshot.PersonID); err != nil {
			return err
		}
		current, err := getEmploymentForUpdateTx(ctx, tx, s.dialect, id)
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return ErrEmploymentRevisionConflict
		}
		if !current.IsCurrent {
			return fmt.Errorf("%w: a historical employment cannot be primary", ErrEmploymentInvalid)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE employments SET is_primary = ?, revision = revision + 1, updated_at = %s WHERE person_id = ? AND id <> ? AND %s`, s.dialect.Now(), s.dialect.BoolTrueExpr("is_primary")), false, current.PersonID, id); err != nil {
			return fmt.Errorf("demote primary employment: %w", err)
		}
		employment, err = scanEmployment(tx.QueryRowContext(ctx, fmt.Sprintf(`UPDATE employments SET is_primary = ?, revision = revision + 1, updated_at = %s WHERE id = ? AND revision = ? RETURNING %s`, s.dialect.Now(), employmentColumns), true, id, expectedRevision))
		if errors.Is(err, sql.ErrNoRows) {
			return s.employmentCASMissTx(ctx, tx, id)
		}
		if err != nil {
			return fmt.Errorf("promote employment %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return employment, nil
}

func (s *Store) DeleteEmploymentContext(ctx context.Context, id, expectedRevision int64) error {
	return retryContendedWriteErr(ctx, s, "delete employment", func() error {
		return s.deleteEmploymentOnce(ctx, id, expectedRevision)
	})
}

func (s *Store) deleteEmploymentOnce(ctx context.Context, id, expectedRevision int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		snapshot, err := getEmploymentTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if snapshot.Revision != expectedRevision {
			return ErrEmploymentRevisionConflict
		}
		if err := s.claimEmploymentPeopleTx(ctx, tx, snapshot.PersonID); err != nil {
			return err
		}
		var deletedID int64
		err = tx.QueryRowContext(ctx,
			`DELETE FROM employments WHERE id = ? AND revision = ? RETURNING id`,
			id, expectedRevision).Scan(&deletedID)
		if errors.Is(err, sql.ErrNoRows) {
			return s.employmentCASMissTx(ctx, tx, id)
		}
		if err != nil {
			return fmt.Errorf("delete employment %d: %w", id, err)
		}
		return nil
	})
}

func (s *Store) ListEmploymentsContext(ctx context.Context, filter EmploymentFilter) ([]Employment, error) {
	return s.listEmploymentsContext(ctx, s.db, filter)
}

func (s *Store) listEmploymentsContext(
	ctx context.Context, queryer contextRowsQuerier,
	filter EmploymentFilter,
) ([]Employment, error) {
	return s.queryEmploymentsContext(ctx, queryer, filter, true)
}

// listAllEmploymentsContext returns every match in one query, ignoring the
// filter's page bounds. Callers whose result doubles as a retention set — the
// vCard projection snapshot, where an omitted row means a deleted property —
// must not read a page.
func (s *Store) listAllEmploymentsContext(
	ctx context.Context, queryer contextRowsQuerier,
	filter EmploymentFilter,
) ([]Employment, error) {
	return s.queryEmploymentsContext(ctx, queryer, filter, false)
}

func (s *Store) queryEmploymentsContext(
	ctx context.Context, queryer contextRowsQuerier,
	filter EmploymentFilter, paginated bool,
) ([]Employment, error) {
	if filter.PersonID <= 0 && filter.OrganizationID <= 0 {
		return nil, fmt.Errorf("%w: person id or organization id is required", ErrEmploymentInvalid)
	}
	conditions, args := make([]string, 0, 3), make([]any, 0, 4)
	if filter.PersonID > 0 {
		conditions, args = append(conditions, "person_id = ?"), append(args, filter.PersonID)
	}
	if filter.OrganizationID > 0 {
		conditions, args = append(conditions, "organization_id = ?"), append(args, filter.OrganizationID)
	}
	current := s.dialect.BoolTrueExpr("is_current")
	if filter.CurrentOnly {
		conditions = append(conditions, current)
	}
	query := `SELECT ` + employmentColumns + ` FROM employments WHERE ` + strings.Join(conditions, " AND ") + fmt.Sprintf(` ORDER BY
		CASE WHEN %s THEN 0 ELSE 1 END, CASE WHEN %s THEN 0 ELSE 1 END,
		COALESCE(end_year, 9999) DESC, COALESCE(end_month, 12) DESC, COALESCE(end_day, 31) DESC,
		COALESCE(start_year, 0) DESC, COALESCE(start_month, 1) DESC, COALESCE(start_day, 1) DESC, id`, current, s.dialect.BoolTrueExpr("is_primary"))
	if paginated {
		limit := filter.Limit
		if limit <= 0 {
			limit = DefaultEmploymentPageSize
		} else if limit > MaxEmploymentPageSize {
			limit = MaxEmploymentPageSize
		}
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, max(filter.Offset, 0))
	}
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list employments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	employments := make([]Employment, 0)
	for rows.Next() {
		employment, err := scanEmployment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan employment: %w", err)
		}
		employments = append(employments, *employment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list employments: %w", err)
	}
	return employments, nil
}

func NormalizeEmploymentTitle(title *string) string {
	if title == nil {
		return ""
	}
	return strings.ToLower(strings.Join(strings.Fields(*title), " "))
}

func validateEmploymentInput(input EmploymentInput) (EmploymentInput, error) {
	if input.PersonID <= 0 {
		return input, fmt.Errorf("%w: person id must be positive", ErrEmploymentInvalid)
	}
	if input.OrganizationID <= 0 {
		return input, fmt.Errorf("%w: organization id must be positive", ErrEmploymentInvalid)
	}
	input.Title = trimEmploymentString(input.Title)
	input.Role = trimEmploymentString(input.Role)
	input.Department = trimEmploymentString(input.Department)
	input.Location = trimEmploymentString(input.Location)
	input.Description = trimEmploymentString(input.Description)
	input.SourceRef = trimEmploymentString(input.SourceRef)
	if input.Source == "" {
		input.Source = ProvenanceUser
	} else {
		source, err := ParseProvenance(string(input.Source))
		if err != nil {
			return input, err
		}
		input.Source = source
	}
	if input.Confidence != nil && (*input.Confidence < 0 || *input.Confidence > 1) {
		return input, fmt.Errorf("%w: confidence must be between 0 and 1", ErrEmploymentInvalid)
	}
	if input.Confidence != nil && input.Source.IsDeclared() {
		return input, fmt.Errorf("%w: confidence is only meaningful for derived or suggested values", ErrEmploymentInvalid)
	}
	if input.StartDate != nil {
		if err := validateEmploymentDate(*input.StartDate, "start"); err != nil {
			return input, err
		}
	}
	if input.EndDate != nil {
		if err := validateEmploymentDate(*input.EndDate, "end"); err != nil {
			return input, err
		}
	}
	if input.StartDate != nil && input.EndDate != nil && CompareAtSharedPrecision(*input.EndDate, *input.StartDate) < 0 {
		return input, fmt.Errorf("%w: end date must not precede start date", ErrEmploymentInvalid)
	}
	if input.IsCurrent != nil && *input.IsCurrent && input.EndDate != nil {
		return input, fmt.Errorf("%w: a current employment cannot have an end date", ErrEmploymentInvalid)
	}
	return input, nil
}

func validateEmploymentDate(date PartialDate, label string) error {
	if err := date.Validate(); err != nil {
		return fmt.Errorf("%w: %s date is invalid: %w", ErrEmploymentInvalid, label, err)
	}
	if date.Year == nil {
		return fmt.Errorf("%w: %s date requires a year", ErrEmploymentInvalid, label)
	}
	return nil
}
func trimEmploymentString(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
func employmentCurrent(input EmploymentInput) bool {
	return input.IsCurrent == nil && input.EndDate == nil || input.IsCurrent != nil && *input.IsCurrent
}

func (s *Store) verifyEmploymentReferencesTx(ctx context.Context, tx *loggedTx, input EmploymentInput) error {
	var exists bool
	organization, err := getOrganizationForUpdateTx(
		ctx, tx, s.dialect, input.OrganizationID)
	if err != nil {
		return err
	}
	if organization.MergedIntoID != nil {
		return fmt.Errorf("%w: employment cannot target a merged organization",
			ErrOrganizationInvalid)
	}
	if input.AddressID != nil {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM organization_addresses WHERE id = ? AND organization_id = ?)`, *input.AddressID, input.OrganizationID).Scan(&exists); err != nil {
			return fmt.Errorf("verify employment address: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: address %d does not belong to organization %d", ErrEmploymentInvalid, *input.AddressID, input.OrganizationID)
		}
	}
	return nil
}

// claimEmploymentPeopleTx is the single gate every employment mutation passes
// through before touching a row. It locks each affected person in ascending ID
// order so two writes over the same pair cannot deadlock, and bumps their
// vCard projection revision: a person's snapshot carries their employments and
// each employer's full profile, so any employment write changes their
// projection and must be visible to a concurrent envelope commit. Doing both
// here rather than at the five call sites means a sixth employment mutation
// cannot acquire the lock and forget the bump.
func (s *Store) claimEmploymentPeopleTx(
	ctx context.Context, tx *loggedTx, personIDs ...int64,
) error {
	ids := append([]int64(nil), personIDs...)
	slices.Sort(ids)
	var previous int64
	for index, personID := range ids {
		if index > 0 && personID == previous {
			continue
		}
		var lockedID int64
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT id FROM persons WHERE id = ?%s
		`, s.dialect.SelectForUpdate()), personID).Scan(&lockedID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		}
		if err != nil {
			return fmt.Errorf("lock employment person %d: %w", personID, err)
		}
		previous = personID
	}
	return s.bumpPersonVCardProjectionsTx(ctx, tx, ids...)
}

func (s *Store) resolveEmploymentPrimaryTx(ctx context.Context, tx *loggedTx, input EmploymentInput, current bool, excludeID int64) (bool, error) {
	primaryExists, err := s.hasPrimaryCurrentTx(ctx, tx, input.PersonID, excludeID)
	if err != nil {
		return false, err
	}
	if input.IsPrimary == nil {
		return current && !primaryExists, nil
	}
	if !*input.IsPrimary {
		return false, nil
	}
	if !current {
		return false, fmt.Errorf("%w: a historical employment cannot be primary", ErrEmploymentInvalid)
	}
	if primaryExists {
		return false, ErrEmploymentPrimaryConflict
	}
	return true, nil
}

func (s *Store) hasPrimaryCurrentTx(ctx context.Context, tx *loggedTx, personID, excludeID int64) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM employments WHERE person_id = ? AND id <> ? AND `+s.dialect.BoolTrueExpr("is_primary")+` AND `+s.dialect.BoolTrueExpr("is_current")+`)`, personID, excludeID).Scan(&exists)
	return exists, err
}

func (s *Store) insertEmploymentTx(ctx context.Context, tx *loggedTx, input EmploymentInput, current, primary bool) (*Employment, error) {
	employment, err := s.employmentWriteWithConflictSavepointTx(ctx, tx, input, current, 0,
		func() (*Employment, error) {
			return scanEmployment(tx.QueryRowContext(ctx, `INSERT INTO employments (person_id, organization_id, title, title_normalized, role, department, location, address_id, description, start_year, start_month, start_day, end_year, end_month, end_day, is_current, is_primary, source, source_ref, confidence) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING `+employmentColumns, employmentWriteArgs(input, current, primary)...))
		})
	if err != nil {
		if errors.Is(err, ErrEmploymentDuplicateActive) || errors.Is(err, ErrEmploymentPrimaryConflict) {
			return nil, err
		}
		return nil, fmt.Errorf("add employment: %w", err)
	}
	return employment, nil
}

func employmentWriteArgs(input EmploymentInput, current, primary bool) []any {
	start, end := partialDateArgs(input.StartDate), partialDateArgs(input.EndDate)
	return []any{input.PersonID, input.OrganizationID, input.Title, NormalizeEmploymentTitle(input.Title), input.Role, input.Department, input.Location, input.AddressID, input.Description, start[0], start[1], start[2], end[0], end[1], end[2], current, primary, input.Source, input.SourceRef, input.Confidence}
}
func partialDateArgs(date *PartialDate) []any {
	if date == nil {
		return []any{nil, nil, nil}
	}
	return PartialDateArgs(*date)
}

func (s *Store) classifyEmploymentConflictTx(ctx context.Context, tx *loggedTx, input EmploymentInput, current bool, excludeID int64) error {
	if current {
		var duplicate bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM employments WHERE person_id = ? AND organization_id = ? AND title_normalized = ? AND id <> ? AND `+s.dialect.BoolTrueExpr("is_current")+`)`, input.PersonID, input.OrganizationID, NormalizeEmploymentTitle(input.Title), excludeID).Scan(&duplicate)
		if err != nil {
			return fmt.Errorf("classify employment conflict: %w", err)
		}
		if duplicate {
			return ErrEmploymentDuplicateActive
		}
	}
	primary, err := s.hasPrimaryCurrentTx(ctx, tx, input.PersonID, excludeID)
	if err != nil {
		return fmt.Errorf("classify employment conflict: %w", err)
	}
	if primary {
		return ErrEmploymentPrimaryConflict
	}
	return ErrEmploymentDuplicateActive
}

// employmentWriteWithConflictSavepointTx recovers a unique violation before
// asking the database which partial index was hit. PostgreSQL aborts the whole
// transaction after SQLSTATE 23505; rolling back to the savepoint restores a
// queryable transaction while SQLite preserves the same classification path.
func (s *Store) employmentWriteWithConflictSavepointTx(
	ctx context.Context, tx *loggedTx, input EmploymentInput, current bool, excludeID int64,
	write func() (*Employment, error),
) (*Employment, error) {
	const savepoint = "employment_write"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return nil, fmt.Errorf("create employment write savepoint: %w", err)
	}
	employment, err := write()
	if err == nil {
		if _, releaseErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); releaseErr != nil {
			return nil, fmt.Errorf("release employment write savepoint: %w", releaseErr)
		}
		return employment, nil
	}
	if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rollbackErr != nil {
		return nil, fmt.Errorf("rollback employment write savepoint: %w", rollbackErr)
	}
	if _, releaseErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); releaseErr != nil {
		return nil, fmt.Errorf("release rolled-back employment write savepoint: %w", releaseErr)
	}
	if s.dialect.IsConflictError(err) {
		return nil, s.classifyEmploymentConflictTx(ctx, tx, input, current, excludeID)
	}
	return nil, err
}

func (s *Store) employmentCASMissTx(ctx context.Context, tx *loggedTx, id int64) error {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM employments WHERE id = ?`, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEmploymentNotFound
	}
	if err != nil {
		return fmt.Errorf("check employment %d after revision miss: %w", id, err)
	}
	return ErrEmploymentRevisionConflict
}
func getEmploymentTx(ctx context.Context, tx *loggedTx, id int64) (*Employment, error) {
	employment, err := scanEmployment(tx.QueryRowContext(ctx, `SELECT `+employmentColumns+` FROM employments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmploymentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get employment %d: %w", id, err)
	}
	return employment, nil
}

func getEmploymentForUpdateTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, id int64,
) (*Employment, error) {
	employment, err := scanEmployment(tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s FROM employments WHERE id = ?%s
	`, employmentColumns, dialect.SelectForUpdate()), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmploymentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get employment %d for update: %w", id, err)
	}
	return employment, nil
}

func scanEmployment(row scanner) (*Employment, error) {
	var employment Employment
	var title, role, department, location, description, sourceRef sql.NullString
	var addressID sql.NullInt64
	var startYear, startMonth, startDay, endYear, endMonth, endDay sql.NullInt64
	var confidence sql.NullFloat64
	var createdAt, updatedAt sql.NullTime
	err := row.Scan(&employment.ID, &employment.PersonID, &employment.OrganizationID, &title, &role, &department, &location, &addressID, &description, &startYear, &startMonth, &startDay, &endYear, &endMonth, &endDay, &employment.IsCurrent, &employment.IsPrimary, &employment.Source, &sourceRef, &confidence, &employment.Revision, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	employment.Title, employment.Role, employment.Department, employment.Location, employment.Description, employment.SourceRef = nullStringPtr(title), nullStringPtr(role), nullStringPtr(department), nullStringPtr(location), nullStringPtr(description), nullStringPtr(sourceRef)
	if addressID.Valid {
		employment.AddressID = new(addressID.Int64)
	}
	if confidence.Valid {
		employment.Confidence = new(confidence.Float64)
	}
	start, end := ScanPartialDate(startYear, startMonth, startDay), ScanPartialDate(endYear, endMonth, endDay)
	if !start.IsZero() {
		employment.StartDate = &start
	}
	if !end.IsZero() {
		employment.EndDate = &end
	}
	var timeErr error
	employment.CreatedAt, timeErr = requireNullTime(createdAt, "created_at")
	if timeErr != nil {
		return nil, timeErr
	}
	employment.UpdatedAt, timeErr = requireNullTime(updatedAt, "updated_at")
	if timeErr != nil {
		return nil, timeErr
	}
	return &employment, nil
}
