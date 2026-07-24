package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/explorecatalog"
	"go.kenn.io/msgvault/internal/jsonexact"
)

const CurrentSavedViewSchemaVersion = 1

var (
	ErrSavedViewNotFound                 = errors.New("saved view not found")
	ErrSavedViewNameConflict             = errors.New("saved view name already exists")
	ErrSavedViewRevisionConflict         = errors.New("saved view revision conflict")
	ErrSavedViewInvalidState             = errors.New("invalid saved view canonical state")
	ErrSavedViewUnsupportedSchemaVersion = errors.New("unsupported saved view schema version")
)

// SavedViewStateEnvelope is the version-1 canonical analytical definition.
// Every nested definition is closed and typed so result rows, bulk selection,
// and other transient workspace state cannot hide below an allowed field.
type SavedViewStateEnvelope struct {
	Query           string            `json:"query,omitempty"`
	SearchMode      string            `json:"search_mode,omitempty"`
	Filters         []SavedViewFilter `json:"filters,omitempty"`
	Grouping        []string          `json:"grouping,omitempty"`
	Presentation    string            `json:"presentation,omitempty"`
	Sort            []SavedViewSort   `json:"sort,omitempty"`
	Columns         []string          `json:"columns,omitempty"`
	InspectorPinned bool              `json:"inspector_pinned,omitempty"`
}

// SavedViewFilter is a normalized version-1 analytical predicate. Values are
// strings deliberately: exact numeric identifiers remain lossless in both Go
// and JavaScript clients, including values above JavaScript's safe integer
// range.
type SavedViewFilter struct {
	Field    string   `json:"field"`
	Operator string   `json:"operator"`
	Values   []string `json:"values" doc:"Exact filter values; numeric identifiers use decimal strings"`
}

type SavedViewSort struct {
	Field     string `json:"field"`
	Direction string `json:"direction" enum:"asc,desc"`
}

type SavedView struct {
	ID             int64           `json:"id"`
	Name           string          `json:"name"`
	Description    *string         `json:"description,omitempty"`
	CanonicalState json.RawMessage `json:"canonical_state"`
	SchemaVersion  int             `json:"schema_version"`
	Revision       int64           `json:"revision"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type SavedViewInput struct {
	Name           string
	Description    *string
	CanonicalState json.RawMessage
	SchemaVersion  int
}

func (s *Store) CreateSavedView(ctx context.Context, input SavedViewInput) (*SavedView, error) {
	validated, err := validateSavedViewInput(input)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`
		INSERT INTO saved_views (name, description, canonical_state, schema_version)
		VALUES (?, ?, %s, ?)
		RETURNING id, name, description, canonical_state, schema_version,
		          revision, created_at, updated_at
	`, s.dialect.JSONBindExpr())
	view, err := scanSavedView(s.db.QueryRowContext(ctx, query,
		validated.Name, validated.Description, string(validated.CanonicalState), validated.SchemaVersion))
	if err != nil {
		if s.dialect.IsConflictError(err) {
			return nil, ErrSavedViewNameConflict
		}
		return nil, fmt.Errorf("create saved view: %w", err)
	}
	return view, nil
}

func (s *Store) ListSavedViews(ctx context.Context) ([]SavedView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, canonical_state, schema_version,
		       revision, created_at, updated_at
		FROM saved_views
		ORDER BY LOWER(name), id
	`)
	if err != nil {
		return nil, fmt.Errorf("list saved views: %w", err)
	}
	defer func() { _ = rows.Close() }()

	views := make([]SavedView, 0)
	for rows.Next() {
		view, scanErr := scanSavedView(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan saved view: %w", scanErr)
		}
		views = append(views, *view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate saved views: %w", err)
	}
	return views, nil
}

func (s *Store) GetSavedView(ctx context.Context, id int64) (*SavedView, error) {
	view, err := scanSavedView(s.db.QueryRowContext(ctx, `
		SELECT id, name, description, canonical_state, schema_version,
		       revision, created_at, updated_at
		FROM saved_views
		WHERE id = ?
	`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSavedViewNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get saved view %d: %w", id, err)
	}
	return view, nil
}

func (s *Store) UpdateSavedView(
	ctx context.Context, id, expectedRevision int64, input SavedViewInput,
) (*SavedView, error) {
	validated, err := validateSavedViewInput(input)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`
		UPDATE saved_views
		SET name = ?, description = ?, canonical_state = %s, schema_version = ?,
		    revision = revision + 1, updated_at = %s
		WHERE id = ? AND revision = ?
		RETURNING id, name, description, canonical_state, schema_version,
		          revision, created_at, updated_at
	`, s.dialect.JSONBindExpr(), s.dialect.Now())
	view, err := scanSavedView(s.db.QueryRowContext(ctx, query,
		validated.Name, validated.Description, string(validated.CanonicalState), validated.SchemaVersion,
		id, expectedRevision))
	if err == nil {
		return view, nil
	}
	if s.dialect.IsConflictError(err) {
		return nil, ErrSavedViewNameConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("update saved view %d: %w", id, err)
	}
	return nil, s.savedViewCASMiss(ctx, id)
}

func (s *Store) DeleteSavedView(ctx context.Context, id, expectedRevision int64) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM saved_views WHERE id = ? AND revision = ?`, id, expectedRevision)
	if err != nil {
		return fmt.Errorf("delete saved view %d: %w", id, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read saved view delete result: %w", err)
	}
	if deleted == 1 {
		return nil
	}
	return s.savedViewCASMiss(ctx, id)
}

func (s *Store) savedViewCASMiss(ctx context.Context, id int64) error {
	var revision int64
	err := s.db.QueryRowContext(ctx, `SELECT revision FROM saved_views WHERE id = ?`, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSavedViewNotFound
	}
	if err != nil {
		return fmt.Errorf("check saved view %d after revision miss: %w", id, err)
	}
	return ErrSavedViewRevisionConflict
}

func validateSavedViewInput(input SavedViewInput) (SavedViewInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return SavedViewInput{}, fmt.Errorf("%w: name is required", ErrSavedViewInvalidState)
	}
	if input.SchemaVersion != CurrentSavedViewSchemaVersion {
		return SavedViewInput{}, fmt.Errorf("%w: got %d, support %d",
			ErrSavedViewUnsupportedSchemaVersion, input.SchemaVersion, CurrentSavedViewSchemaVersion)
	}

	trimmedState := bytes.TrimSpace(input.CanonicalState)
	if len(trimmedState) == 0 || bytes.Equal(trimmedState, []byte("null")) {
		return SavedViewInput{}, fmt.Errorf("%w: canonical state must be a JSON object", ErrSavedViewInvalidState)
	}
	if err := jsonexact.Validate(trimmedState, SavedViewStateEnvelope{}); err != nil {
		return SavedViewInput{}, fmt.Errorf("%w: %w", ErrSavedViewInvalidState, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmedState))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var envelope SavedViewStateEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return SavedViewInput{}, fmt.Errorf("%w: %w", ErrSavedViewInvalidState, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SavedViewInput{}, fmt.Errorf("%w: canonical state must contain one JSON object", ErrSavedViewInvalidState)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmedState, &fields); err != nil || fields == nil {
		return SavedViewInput{}, fmt.Errorf("%w: canonical state must be a JSON object", ErrSavedViewInvalidState)
	}
	for _, name := range []string{
		"query", "search_mode", "filters", "grouping", "presentation", "sort", "columns", "inspector_pinned",
	} {
		if value, present := fields[name]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return SavedViewInput{}, fmt.Errorf("%w: %s must not be null", ErrSavedViewInvalidState, name)
		}
	}
	if rawFilters, present := fields["filters"]; present {
		var filters []struct {
			Values []json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(rawFilters, &filters); err == nil {
			for filterIndex, filter := range filters {
				for valueIndex, value := range filter.Values {
					if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
						return SavedViewInput{}, fmt.Errorf(
							"%w: filters[%d].values[%d] must not be null",
							ErrSavedViewInvalidState, filterIndex, valueIndex,
						)
					}
				}
			}
		}
	}
	if err := validateSavedViewEnvelope(envelope); err != nil {
		return SavedViewInput{}, fmt.Errorf("%w: %w", ErrSavedViewInvalidState, err)
	}
	canonical, err := json.Marshal(envelope)
	if err != nil {
		return SavedViewInput{}, fmt.Errorf("%w: %w", ErrSavedViewInvalidState, err)
	}
	input.CanonicalState = canonical
	return input, nil
}

func validateSavedViewEnvelope(envelope SavedViewStateEnvelope) error {
	for i, filter := range envelope.Filters {
		if strings.TrimSpace(filter.Field) == "" {
			return fmt.Errorf("filters[%d].field is required", i)
		}
		if strings.TrimSpace(filter.Operator) == "" {
			return fmt.Errorf("filters[%d].operator is required", i)
		}
		if filter.Values == nil {
			return fmt.Errorf("filters[%d].values is required", i)
		}
	}
	for i, field := range envelope.Grouping {
		if !explorecatalog.IsGroupingDimension(field) {
			return fmt.Errorf("grouping[%d] is not a supported analytical dimension", i)
		}
	}
	if envelope.Presentation != "" &&
		envelope.Presentation != "table" &&
		envelope.Presentation != "timeline" &&
		envelope.Presentation != "files" {
		return errors.New("presentation must be table, timeline, or files")
	}
	for i, sort := range envelope.Sort {
		if strings.TrimSpace(sort.Field) == "" {
			return fmt.Errorf("sort[%d].field is required", i)
		}
		if sort.Direction != "asc" && sort.Direction != "desc" {
			return fmt.Errorf("sort[%d].direction must be asc or desc", i)
		}
	}
	for i, column := range envelope.Columns {
		if strings.TrimSpace(column) == "" {
			return fmt.Errorf("columns[%d] must not be empty", i)
		}
	}
	return nil
}

func scanSavedView(row scanner) (*SavedView, error) {
	var (
		view        SavedView
		description sql.NullString
		state       []byte
	)
	if err := row.Scan(
		&view.ID, &view.Name, &description, &state, &view.SchemaVersion,
		&view.Revision, &view.CreatedAt, &view.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if description.Valid {
		view.Description = &description.String
	}
	view.CanonicalState = json.RawMessage(append([]byte(nil), state...))
	return &view, nil
}
