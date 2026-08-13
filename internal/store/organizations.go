package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

var (
	ErrOrganizationNotFound         = errors.New("organization not found")
	ErrOrganizationRevisionConflict = errors.New("organization revision conflict")
	ErrOrganizationInvalid          = errors.New("invalid organization")
	ErrOrganizationHasEmployments   = errors.New("organization still has employment records")
	ErrOrganizationMergeConflict    = errors.New("organization merge conflict")
)

// OrganizationKind classifies an organization for presentation.
type OrganizationKind string

const (
	OrganizationKindCompany    OrganizationKind = "company"
	OrganizationKindNonprofit  OrganizationKind = "nonprofit"
	OrganizationKindSchool     OrganizationKind = "school"
	OrganizationKindGovernment OrganizationKind = "government"
	OrganizationKindHousehold  OrganizationKind = "household"
	OrganizationKindOther      OrganizationKind = "other"
)

// AllOrganizationKinds is the ordered organization-kind vocabulary.
var AllOrganizationKinds = []OrganizationKind{
	OrganizationKindCompany,
	OrganizationKindNonprofit,
	OrganizationKindSchool,
	OrganizationKindGovernment,
	OrganizationKindHousehold,
	OrganizationKindOther,
}

// MergeOrganizationsContext folds a losing organization into a survivor
// without deleting employment history. The losing organization's active
// profile rows and typed attribute values are superseded rather than moved:
// the redirect is immutable afterwards, so anything left active there could
// never be corrected or cleared, and stale record references would block
// referenced-person deletion forever. The superseded rows remain readable as
// history; facts that should live on the survivor are curated there
// explicitly rather than migrated blindly.
func (s *Store) MergeOrganizationsContext(
	ctx context.Context, survivorID, survivorRevision, losingID, losingRevision int64,
) (*Organization, error) {
	if survivorID <= 0 || losingID <= 0 {
		return nil, fmt.Errorf("%w: organization id must be positive", ErrOrganizationInvalid)
	}
	if survivorID == losingID {
		return nil, fmt.Errorf("%w: cannot merge an organization into itself",
			ErrOrganizationInvalid)
	}

	var survivor *Organization
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		firstID, secondID := survivorID, losingID
		if firstID > secondID {
			firstID, secondID = secondID, firstID
		}
		first, err := getOrganizationForUpdateTx(ctx, tx, s.dialect, firstID)
		if err != nil {
			return err
		}
		second, err := getOrganizationForUpdateTx(ctx, tx, s.dialect, secondID)
		if err != nil {
			return err
		}
		lockedSurvivor, lockedLosing := first, second
		if survivorID != firstID {
			lockedSurvivor, lockedLosing = second, first
		}
		if lockedSurvivor.Revision != survivorRevision ||
			lockedLosing.Revision != losingRevision {
			return ErrOrganizationRevisionConflict
		}
		if lockedSurvivor.MergedIntoID != nil {
			return fmt.Errorf("%w: cannot merge into an already merged organization",
				ErrOrganizationInvalid)
		}
		if lockedLosing.MergedIntoID != nil {
			return fmt.Errorf("%w: cannot re-merge an already merged organization",
				ErrOrganizationInvalid)
		}
		if lockedSurvivor.RetiredAt != nil {
			return fmt.Errorf("%w: cannot merge into a retired organization",
				ErrOrganizationInvalid)
		}

		var collisions int64
		collisionQuery := `SELECT COUNT(*)
			FROM employments losing_row
			WHERE losing_row.organization_id = ? AND ` +
			s.dialect.BoolTrueExpr("losing_row.is_current") + `
			  AND EXISTS (
				SELECT 1 FROM employments survivor_row
				WHERE survivor_row.organization_id = ?
				  AND survivor_row.person_id = losing_row.person_id
				  AND survivor_row.title_normalized = losing_row.title_normalized
				  AND ` + s.dialect.BoolTrueExpr("survivor_row.is_current") + `
			  )`
		if err := tx.QueryRowContext(ctx, collisionQuery, losingID, survivorID).
			Scan(&collisions); err != nil {
			return fmt.Errorf("check organization merge employment collisions: %w", err)
		}
		if collisions > 0 {
			return fmt.Errorf("%w: person already has a current employment with this "+
				"organization and title", ErrOrganizationMergeConflict)
		}

		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE employments
			SET organization_id = ?, address_id = NULL, revision = revision + 1,
			    updated_at = %s
			WHERE organization_id = ?
		`, s.dialect.Now()), survivorID, losingID); err != nil {
			return fmt.Errorf("repoint organization employments: %w", err)
		}
		now := time.Now().UTC()
		for _, table := range []string{
			"organization_names", "organization_identifiers",
			"organization_addresses", "organization_contact_points",
			"organization_media", "organization_categories",
		} {
			if err := s.supersedeOrganizationRowsTx(
				ctx, tx, table, losingID, nil, now); err != nil {
				return fmt.Errorf("retire merged organization values: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE organization_attribute_values
			SET superseded_at = ?
			WHERE organization_id = ? AND superseded_at IS NULL AND active_from > ?
		`, now, losingID, now); err != nil {
			return fmt.Errorf("retract future merged organization attributes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE organization_attribute_values
			SET active_until = ?, superseded_at = ?
			WHERE organization_id = ? AND active_until IS NULL
			  AND superseded_at IS NULL AND active_from <= ?
		`, now, now, losingID, now); err != nil {
			return fmt.Errorf("retire merged organization attributes: %w", err)
		}
		sourceRef := fmt.Sprintf("organization-merge:%d", losingID)
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`
			INSERT OR IGNORE INTO organization_names (
				organization_id, name_kind, formatted, original_value,
				name_normalized, source, source_ref
			) VALUES (?, 'former', ?, ?, ?, 'system', ?)
		`), survivorID, lockedLosing.Name,
			lockedLosing.Name, NormalizeOrganizationName(lockedLosing.Name),
			sourceRef); err != nil {
			return fmt.Errorf("retain merged organization name: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE organizations
			SET merged_into_id = ?, retired_at = %s, revision = revision + 1,
			    updated_at = %s
			WHERE id = ? AND revision = ?
		`, s.dialect.Now(), s.dialect.Now()),
			survivorID, losingID, losingRevision); err != nil {
			return fmt.Errorf("mark losing organization merged: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE organizations
			SET revision = revision + 1, updated_at = %s
			WHERE id = ? AND revision = ?
		`, s.dialect.Now()), survivorID, survivorRevision); err != nil {
			return fmt.Errorf("bump surviving organization revision: %w", err)
		}
		survivor, err = getOrganizationTx(ctx, tx, survivorID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return survivor, nil
}

// Organization is a durable curated real-world entity.
type Organization struct {
	ID            int64            `json:"id"`
	Name          string           `json:"name"`
	Kind          OrganizationKind `json:"kind"`
	PrimaryDomain *string          `json:"primary_domain,omitempty"`
	Description   *string          `json:"description,omitempty"`
	Revision      int64            `json:"revision"`
	MergedIntoID  *int64           `json:"merged_into_id,omitempty"`
	RetiredAt     *time.Time       `json:"retired_at,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// OrganizationInput is the full mutable organization field set.
type OrganizationInput struct {
	Name          string
	Kind          OrganizationKind
	PrimaryDomain *string
	Description   *string
}

// OrganizationFilter bounds an organization listing.
type OrganizationFilter struct {
	Query          string
	IncludeRetired bool
	Limit          int
	Offset         int
}

const (
	DefaultOrganizationPageSize = 100
	MaxOrganizationPageSize     = 500
)

const organizationColumns = `
	id, name, name_normalized, kind, primary_domain, description,
	revision, merged_into_id, retired_at, created_at, updated_at
`

// CreateOrganizationContext creates a revisioned organization root.
func (s *Store) CreateOrganizationContext(
	ctx context.Context, input OrganizationInput,
) (*Organization, error) {
	input, err := validateOrganizationInput(input)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO organizations (
			name, name_normalized, kind, primary_domain, description
		) VALUES (?, ?, ?, ?, ?)
		RETURNING `+organizationColumns,
		input.Name, NormalizeOrganizationName(input.Name), input.Kind,
		input.PrimaryDomain, input.Description)
	organization, err := scanOrganization(row)
	if err != nil {
		return nil, fmt.Errorf("create organization: %w", err)
	}
	return organization, nil
}

// GetOrganizationContext returns one organization, including retired roots.
func (s *Store) GetOrganizationContext(ctx context.Context, id int64) (*Organization, error) {
	organization, err := scanOrganization(s.db.QueryRowContext(ctx,
		`SELECT `+organizationColumns+` FROM organizations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrganizationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get organization %d: %w", id, err)
	}
	return organization, nil
}

// ListOrganizationsContext returns a bounded, stable organization page.
func (s *Store) ListOrganizationsContext(
	ctx context.Context, filter OrganizationFilter,
) ([]Organization, error) {
	where, args := organizationWhere(filter)
	limit, offset := organizationPage(filter)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `SELECT `+organizationColumns+
		` FROM organizations`+where+` ORDER BY name_normalized, id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	organizations := make([]Organization, 0)
	for rows.Next() {
		organization, err := scanOrganization(rows)
		if err != nil {
			return nil, fmt.Errorf("scan organization: %w", err)
		}
		organizations = append(organizations, *organization)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	return organizations, nil
}

// CountOrganizationsContext counts the same filtered set as the list method.
func (s *Store) CountOrganizationsContext(
	ctx context.Context, filter OrganizationFilter,
) (int64, error) {
	where, args := organizationWhere(filter)
	var count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM organizations`+where, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count organizations: %w", err)
	}
	return count, nil
}

// ReplaceOrganizationContext atomically replaces root fields and lifecycle state.
func (s *Store) ReplaceOrganizationContext(
	ctx context.Context, id, expectedRevision int64, input OrganizationInput, retired bool,
) (*Organization, error) {
	input, err := validateOrganizationInput(input)
	if err != nil {
		return nil, err
	}
	var organization *Organization
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		organization, err = scanOrganization(tx.QueryRowContext(ctx, fmt.Sprintf(`
			UPDATE organizations
			SET name = ?, name_normalized = ?, kind = ?, primary_domain = ?,
			    description = ?,
			    retired_at = CASE WHEN ? THEN COALESCE(retired_at, %s) ELSE NULL END,
			    revision = revision + 1, updated_at = %s
			WHERE id = ? AND revision = ? AND merged_into_id IS NULL
			RETURNING %s
		`, s.dialect.Now(), s.dialect.Now(), organizationColumns),
			input.Name, NormalizeOrganizationName(input.Name), input.Kind,
			input.PrimaryDomain, input.Description, retired, id, expectedRevision))
		if errors.Is(err, sql.ErrNoRows) {
			return organizationMutableCASMissTx(ctx, tx, id, expectedRevision)
		}
		if err != nil {
			return fmt.Errorf("replace organization %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return organization, nil
}

// DeleteOrganizationContext hard-deletes an unreferenced organization.
func (s *Store) DeleteOrganizationContext(
	ctx context.Context, id, expectedRevision int64,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := getOrganizationForUpdateTx(ctx, tx, s.dialect, id)
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return ErrOrganizationRevisionConflict
		}
		if current.MergedIntoID != nil {
			return fmt.Errorf("%w: cannot delete a merged organization redirect",
				ErrOrganizationInvalid)
		}
		var employments int64
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM employments WHERE organization_id = ?`, id).Scan(&employments)
		if err != nil {
			return fmt.Errorf("count organization %d employments: %w", id, err)
		}
		if employments > 0 {
			return ErrOrganizationHasEmployments
		}
		var redirects int64
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM organizations WHERE merged_into_id = ?`, id).Scan(&redirects)
		if err != nil {
			return fmt.Errorf("count organization %d merge redirects: %w", id, err)
		}
		if redirects > 0 {
			return fmt.Errorf(
				"%w: %d merged organization(s) redirect here; the redirects preserve merge history",
				ErrOrganizationInvalid, redirects)
		}
		var deletedID int64
		err = tx.QueryRowContext(ctx,
			`DELETE FROM organizations WHERE id = ? AND revision = ? RETURNING id`,
			id, expectedRevision).Scan(&deletedID)
		if errors.Is(err, sql.ErrNoRows) {
			return organizationCASMissTx(ctx, tx, id)
		}
		if err != nil {
			return fmt.Errorf("delete organization %d: %w", id, err)
		}
		return nil
	})
}

func organizationCASMissTx(ctx context.Context, tx *loggedTx, id int64) error {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM organizations WHERE id = ?`, id).
		Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOrganizationNotFound
	}
	if err != nil {
		return fmt.Errorf("check organization %d after revision miss: %w", id, err)
	}
	return ErrOrganizationRevisionConflict
}

func organizationMutableCASMissTx(
	ctx context.Context, tx *loggedTx, id, expectedRevision int64,
) error {
	current, err := getOrganizationTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if current.Revision != expectedRevision {
		return ErrOrganizationRevisionConflict
	}
	if current.MergedIntoID != nil {
		return fmt.Errorf("%w: cannot mutate a merged organization redirect",
			ErrOrganizationInvalid)
	}
	return ErrOrganizationRevisionConflict
}

func getOrganizationTx(
	ctx context.Context, tx *loggedTx, id int64,
) (*Organization, error) {
	organization, err := scanOrganization(tx.QueryRowContext(ctx,
		`SELECT `+organizationColumns+` FROM organizations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrganizationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get organization %d: %w", id, err)
	}
	return organization, nil
}

func getOrganizationForUpdateTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, id int64,
) (*Organization, error) {
	organization, err := scanOrganization(tx.QueryRowContext(ctx,
		`SELECT `+organizationColumns+` FROM organizations WHERE id = ?`+
			dialect.SelectForUpdate(), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrganizationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock organization %d: %w", id, err)
	}
	return organization, nil
}

func scanOrganization(row scanner) (*Organization, error) {
	var (
		organization               Organization
		nameNormalized             string
		primaryDomain, description sql.NullString
		mergedIntoID               sql.NullInt64
		retiredAt                  sql.NullTime
	)
	if err := row.Scan(
		&organization.ID, &organization.Name, &nameNormalized, &organization.Kind,
		&primaryDomain, &description, &organization.Revision, &mergedIntoID,
		&retiredAt, &organization.CreatedAt, &organization.UpdatedAt,
	); err != nil {
		return nil, err
	}
	organization.PrimaryDomain = nullStringPtr(primaryDomain)
	organization.Description = nullStringPtr(description)
	if mergedIntoID.Valid {
		organization.MergedIntoID = new(mergedIntoID.Int64)
	}
	if retiredAt.Valid {
		organization.RetiredAt = new(retiredAt.Time)
	}
	return &organization, nil
}

func validateOrganizationInput(input OrganizationInput) (OrganizationInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return OrganizationInput{}, fmt.Errorf("%w: name is required", ErrOrganizationInvalid)
	}
	if input.Kind == "" {
		input.Kind = OrganizationKindOther
	}
	if !slices.Contains(AllOrganizationKinds, input.Kind) {
		return OrganizationInput{}, fmt.Errorf("%w: unknown kind %q",
			ErrOrganizationInvalid, input.Kind)
	}
	if input.PrimaryDomain != nil {
		raw := strings.TrimSpace(*input.PrimaryDomain)
		if raw == "" {
			input.PrimaryDomain = nil
		} else {
			domain := NormalizeDomain(raw)
			if domain == "" {
				return OrganizationInput{}, fmt.Errorf(
					"%w: primary domain %q is not a valid domain", ErrOrganizationInvalid, raw)
			}
			input.PrimaryDomain = &domain
		}
	}
	if input.Description != nil {
		description := strings.TrimSpace(*input.Description)
		if description == "" {
			input.Description = nil
		} else {
			input.Description = &description
		}
	}
	return input, nil
}

func organizationWhere(filter OrganizationFilter) (string, []any) {
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 1)
	if !filter.IncludeRetired {
		conditions = append(conditions, "retired_at IS NULL AND merged_into_id IS NULL")
	}
	if query := NormalizeOrganizationName(filter.Query); query != "" {
		query = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
		conditions = append(conditions, `name_normalized LIKE ? ESCAPE '\'`)
		args = append(args, query+"%")
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func organizationPage(filter OrganizationFilter) (int, int) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultOrganizationPageSize
	} else if limit > MaxOrganizationPageSize {
		limit = MaxOrganizationPageSize
	}
	return limit, max(filter.Offset, 0)
}

// NormalizeOrganizationName returns a case-folded whitespace-normalized key.
func NormalizeOrganizationName(raw string) string {
	return strings.ToLower(strings.Join(strings.Fields(raw), " "))
}

// NormalizeDomain reduces a domain, URL, or email to a bare lowercase host.
func NormalizeDomain(raw string) string {
	candidate := strings.ToLower(strings.TrimSpace(raw))
	if at := strings.LastIndex(candidate, "@"); at >= 0 {
		candidate = candidate[at+1:]
	}
	var host string
	if strings.Contains(candidate, "://") {
		parsed, err := url.Parse(candidate)
		if err != nil {
			return ""
		}
		host = parsed.Hostname()
	} else {
		if strings.Contains(candidate, "/") {
			return ""
		}
		parsed, err := url.Parse("//" + candidate)
		if err != nil {
			return ""
		}
		host = parsed.Hostname()
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "www."), ".")
	if !strings.Contains(host, ".") || strings.ContainsAny(host, "/ \t\r\n") {
		return ""
	}
	return host
}
