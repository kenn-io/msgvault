package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonContactPoint struct {
	Envelope             ValueEnvelope      `json:"envelope"`
	PersonID             int64              `json:"person_id"`
	AddressKind          ContactAddressKind `json:"address_kind"`
	ServiceSlug          *string            `json:"service_slug,omitempty"`
	ScopeKind            *string            `json:"scope_kind,omitempty"`
	ScopeValue           *string            `json:"scope_value,omitempty"`
	OriginalValue        string             `json:"original_value"`
	NormalizedValue      string             `json:"normalized_value"`
	Normalization        string             `json:"normalization"`
	NormalizationVersion int                `json:"normalization_version"`
	URI                  *string            `json:"uri,omitempty"`
}

type PersonContactPointInput struct {
	AddressKind   ContactAddressKind `json:"address_kind"`
	ServiceSlug   *string            `json:"service_slug,omitempty"`
	ScopeKind     *string            `json:"scope_kind,omitempty"`
	ScopeValue    *string            `json:"scope_value,omitempty"`
	OriginalValue string             `json:"original_value"`
	URI           *string            `json:"uri,omitempty"`
	Envelope      ValueEnvelopeInput `json:"envelope"`
}

type ContactPointQuery struct {
	AddressKind     ContactAddressKind
	ServiceSlug     *string
	ScopeKind       *string
	ScopeValue      *string
	NormalizedValue string
}

var (
	ErrInvalidContactAddressKind = errors.New("invalid contact address kind")
	ErrContactPointValueMissing  = errors.New("contact point requires a non-empty value")
)

func (s *Store) AddPersonContactPointContext(
	ctx context.Context, personID int64, input PersonContactPointInput,
) (*PersonContactPoint, error) {
	var result *PersonContactPoint
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = s.addPersonContactPointTx(ctx, tx, personID, input)
		if err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) ListPersonContactPointsContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonContactPoint, error) {
	var points []PersonContactPoint
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		points, err = s.listPersonContactPointsTx(ctx, tx, personID, currentOnly)
		return err
	})
	return points, err
}

func (s *Store) FindPersonContactPointsContext(
	ctx context.Context, query ContactPointQuery,
) ([]PersonContactPoint, error) {
	if !query.AddressKind.Valid() {
		return nil, ErrInvalidContactAddressKind
	}
	query.ScopeKind = trimmedOrNil(query.ScopeKind)
	query.ScopeValue = trimmedOrNil(query.ScopeValue)
	service, hasService, err := s.resolveOptionalCommunicationServiceContext(ctx, query.ServiceSlug)
	if err != nil {
		return nil, err
	}
	var serviceID any
	if hasService {
		serviceID = service.ID
	}
	rows, err := s.db.QueryContext(ctx, personContactPointSelect+`
		WHERE p.address_kind = ?
		  AND (p.service_id = ? OR (p.service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND (p.scope_kind = ? OR (p.scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND (p.scope_value = ? OR (p.scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
		  AND p.normalized_value = ?
		  AND p.active_until IS NULL AND p.superseded_at IS NULL
		ORDER BY p.person_id, p.id`,
		query.AddressKind,
		serviceID, serviceID,
		stringValue(query.ScopeKind), stringValue(query.ScopeKind),
		stringValue(query.ScopeValue), stringValue(query.ScopeValue),
		query.NormalizedValue,
	)
	if err != nil {
		return nil, fmt.Errorf("find person contact points: %w", err)
	}
	defer func() { _ = rows.Close() }()
	points := make([]PersonContactPoint, 0)
	for rows.Next() {
		point, err := scanPersonContactPoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person contact point: %w", err)
		}
		points = append(points, *point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find person contact points: %w", err)
	}
	return points, nil
}

func (s *Store) SupersedePersonContactPointContext(
	ctx context.Context, personID, contactPointID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.supersedePersonContactPointTx(
			ctx, tx, personID, contactPointID, activeUntil,
		); err != nil {
			return err
		}
		return s.bumpPersonRevisionsTx(ctx, tx, personID)
	})
}

func (s *Store) addPersonContactPointTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	input PersonContactPointInput,
) (*PersonContactPoint, error) {
	if !input.AddressKind.Valid() {
		return nil, ErrInvalidContactAddressKind
	}
	if strings.TrimSpace(input.OriginalValue) == "" {
		return nil, ErrContactPointValueMissing
	}
	input.ScopeKind = trimmedOrNil(input.ScopeKind)
	input.ScopeValue = trimmedOrNil(input.ScopeValue)
	service, hasService, err := resolveCommunicationServiceTx(ctx, tx, input.ServiceSlug)
	if err != nil {
		return nil, err
	}
	if err := ValidateServiceScope(service, input.ScopeKind, input.ScopeValue); err != nil {
		return nil, err
	}
	normalized, err := NormalizeServiceValue(service, input.AddressKind, input.OriginalValue)
	if err != nil {
		return nil, err
	}
	normalization := fallbackContactNormalization(input.AddressKind)
	version := 1
	var serviceID any
	if hasService {
		serviceID, normalization, version = service.ID, service.Normalization, service.NormalizationVersion
	}
	env, err := resolveProfileEnvelopeTx(
		ctx, tx, "person_contact_points", "address_kind",
		personID, input.AddressKind, input.Envelope,
	)
	if err != nil {
		return nil, err
	}
	args := []any{
		personID, input.AddressKind, serviceID, stringValue(input.ScopeKind),
		stringValue(input.ScopeValue), input.OriginalValue, normalized,
		normalization, version, stringValue(input.URI),
	}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_contact_points (
		person_id, address_kind, service_id, scope_kind, scope_value,
		original_value, normalized_value, normalization,
		normalization_version, uri, `+profileEnvelopeWriteColumns+`,
		created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf(
			"add person contact point property=%q prop_id=%v: %w",
			env.VCard.Property, env.VCard.PropID, err,
		)
	}
	return getPersonContactPointTx(ctx, tx, personID, id)
}

func (s *Store) listPersonContactPointsTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonContactPoint, error) {
	query := personContactPointSelect + ` WHERE p.person_id = ?`
	if currentOnly {
		query += ` AND p.active_until IS NULL AND p.superseded_at IS NULL`
	}
	query += ` ORDER BY p.address_kind,
		CASE WHEN p.pref IS NULL THEN 1 ELSE 0 END, p.pref, p.ordinal, p.id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonContactPoint, personID)
}

func (s *Store) supersedePersonContactPointTx(
	ctx context.Context,
	tx *loggedTx,
	personID, contactPointID int64,
	activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_contact_points", personID, contactPointID, activeUntil,
	)
}

func fallbackContactNormalization(kind ContactAddressKind) string {
	switch kind {
	case ContactAddressEmail:
		return NormalizationEmail
	case ContactAddressPhone:
		return NormalizationPhoneE164
	case ContactAddressLanguage:
		return NormalizationLower
	default:
		return NormalizationNone
	}
}

func (s *Store) resolveOptionalCommunicationServiceContext(
	ctx context.Context, slug *string,
) (*CommunicationService, bool, error) {
	if slug == nil || strings.TrimSpace(*slug) == "" {
		return nil, false, nil
	}
	service, err := s.ResolveCommunicationServiceContext(ctx, *slug)
	if err != nil {
		return nil, false, err
	}
	return service, true, nil
}

func resolveCommunicationServiceTx(
	ctx context.Context, tx *loggedTx, slug *string,
) (*CommunicationService, bool, error) {
	if slug == nil || strings.TrimSpace(*slug) == "" {
		return nil, false, nil
	}
	lookup := strings.ToLower(strings.TrimSpace(*slug))
	service, err := scanCommunicationService(tx.QueryRowContext(ctx, serviceSelect+`
		WHERE slug = ? OR id = (
			SELECT service_id FROM communication_service_aliases WHERE alias = ?
		)
		ORDER BY CASE WHEN slug = ? THEN 0 ELSE 1 END
		LIMIT 1`, lookup, lookup, lookup))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrServiceNotFound
	}
	if err != nil {
		return nil, false, err
	}
	service.Aliases, err = loadServiceAliasesTx(ctx, tx, service.ID)
	return service, true, err
}

const personContactPointSelect = `SELECT
	p.id, p.person_id, p.address_kind, cs.slug, p.scope_kind, p.scope_value,
	p.original_value, p.normalized_value, p.normalization,
	p.normalization_version, p.uri,
	p.pref, p.ordinal, p.type_label, p.type_tokens, p.vcard_property,
	p.vcard_group, p.vcard_prop_id, p.vcard_pid, p.vcard_altid, p.source,
	p.source_ref, p.confidence, p.active_from, p.active_until,
	p.created_at, p.updated_at, p.superseded_at
	FROM person_contact_points p
	LEFT JOIN communication_services cs ON cs.id = p.service_id`

func getPersonContactPointTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonContactPoint, error) {
	point, err := scanPersonContactPoint(tx.QueryRowContext(ctx,
		personContactPointSelect+` WHERE p.person_id = ? AND p.id = ?`,
		personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person contact point: %w", err)
	}
	return point, nil
}

func scanPersonContactPoint(row scanner) (*PersonContactPoint, error) {
	var point PersonContactPoint
	var serviceSlug, scopeKind, scopeValue, uri sql.NullString
	var env profileEnvelopeScanValues
	dest := []any{
		&point.Envelope.ID, &point.PersonID, &point.AddressKind, &serviceSlug,
		&scopeKind, &scopeValue, &point.OriginalValue, &point.NormalizedValue,
		&point.Normalization, &point.NormalizationVersion, &uri,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	point.ServiceSlug = nullStringPtr(serviceSlug)
	point.ScopeKind = nullStringPtr(scopeKind)
	point.ScopeValue = nullStringPtr(scopeValue)
	point.URI = nullStringPtr(uri)
	if err := env.apply(&point.Envelope); err != nil {
		return nil, err
	}
	return &point, nil
}
