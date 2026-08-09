package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/textimport"
)

const communicationServicesSeedV1 = "communication_services_seed_v1"

const (
	ScopePolicyNone     = "none"
	ScopePolicyOptional = "optional"
	ScopePolicyRequired = "required"
)

const (
	NormalizationNone          = "none"
	NormalizationLower         = "lower"
	NormalizationEmail         = "email"
	NormalizationPhoneE164     = "phone_e164"
	NormalizationStripAtLower  = "strip_at_lower"
	NormalizationByAddressKind = "by_address_kind"
)

var (
	ErrServiceNotFound        = errors.New("communication service not found")
	ErrServiceSlugConflict    = errors.New("communication service slug already exists")
	ErrServiceAliasConflict   = errors.New("communication service alias already maps to another service")
	ErrInvalidServiceSlug     = errors.New("communication service slug must match [a-z0-9][a-z0-9-]*")
	ErrInvalidScopePolicy     = errors.New("invalid communication service scope policy")
	ErrInvalidNormalization   = errors.New("invalid communication service normalization strategy")
	ErrServiceScopeRequired   = errors.New("communication service requires a scope value")
	ErrServiceScopeForbidden  = errors.New("communication service does not accept a scope value")
	ErrServiceScopeIncomplete = errors.New("communication service scope requires both scope kind and scope value")
	ErrNormalizationRejected  = errors.New("value cannot be normalized for this service")
)

type CommunicationService struct {
	ID                   int64     `json:"id"`
	Slug                 string    `json:"slug"`
	DisplayLabel         string    `json:"display_label"`
	Aliases              []string  `json:"aliases"`
	ScopePolicy          string    `json:"scope_policy"`
	DefaultScopeKind     *string   `json:"default_scope_kind,omitempty"`
	Normalization        string    `json:"normalization"`
	NormalizationVersion int       `json:"normalization_version"`
	URIScheme            *string   `json:"uri_scheme,omitempty"`
	ProfileURLTemplate   *string   `json:"profile_url_template,omitempty"`
	IsSystem             bool      `json:"is_system"`
	IsActive             bool      `json:"is_active"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type CommunicationServiceInput struct {
	Slug                 string
	DisplayLabel         string
	Aliases              []string
	ScopePolicy          string
	DefaultScopeKind     *string
	Normalization        string
	NormalizationVersion int
	URIScheme            *string
	ProfileURLTemplate   *string
}

type ContactAddressKind string

const (
	ContactAddressEmail        ContactAddressKind = "email"
	ContactAddressPhone        ContactAddressKind = "phone"
	ContactAddressUsername     ContactAddressKind = "username"
	ContactAddressIMPP         ContactAddressKind = "impp"
	ContactAddressURL          ContactAddressKind = "url"
	ContactAddressSocial       ContactAddressKind = "social"
	ContactAddressCalendar     ContactAddressKind = "calendar"
	ContactAddressContactURI   ContactAddressKind = "contact_uri"
	ContactAddressOrgDirectory ContactAddressKind = "org_directory"
	ContactAddressLanguage     ContactAddressKind = "language"
)

func (k ContactAddressKind) Valid() bool {
	switch k {
	case ContactAddressEmail, ContactAddressPhone, ContactAddressUsername,
		ContactAddressIMPP, ContactAddressURL, ContactAddressSocial,
		ContactAddressCalendar, ContactAddressContactURI,
		ContactAddressOrgDirectory, ContactAddressLanguage:
		return true
	default:
		return false
	}
}

var serviceSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var seededCommunicationServices = []CommunicationServiceInput{
	{Slug: "whatsapp", DisplayLabel: "WhatsApp", ScopePolicy: ScopePolicyNone, Normalization: NormalizationPhoneE164, NormalizationVersion: 1},
	{Slug: "telegram", DisplayLabel: "Telegram", ScopePolicy: ScopePolicyNone, Normalization: NormalizationStripAtLower, NormalizationVersion: 1, URIScheme: new("tg"), ProfileURLTemplate: new("https://t.me/{username}")},
	{Slug: "facebook", DisplayLabel: "Facebook", ScopePolicy: ScopePolicyNone, Normalization: NormalizationStripAtLower, NormalizationVersion: 1, ProfileURLTemplate: new("https://www.facebook.com/{username}")},
	{Slug: "messenger", DisplayLabel: "Messenger", ScopePolicy: ScopePolicyNone, Normalization: NormalizationStripAtLower, NormalizationVersion: 1, ProfileURLTemplate: new("https://m.me/{username}")},
	{Slug: "instagram", DisplayLabel: "Instagram", ScopePolicy: ScopePolicyNone, Normalization: NormalizationStripAtLower, NormalizationVersion: 1, ProfileURLTemplate: new("https://www.instagram.com/{username}")},
	{Slug: "signal", DisplayLabel: "Signal", ScopePolicy: ScopePolicyNone, Normalization: NormalizationPhoneE164, NormalizationVersion: 1},
	{Slug: "x", DisplayLabel: "X", Aliases: []string{"twitter"}, ScopePolicy: ScopePolicyNone, Normalization: NormalizationStripAtLower, NormalizationVersion: 1, ProfileURLTemplate: new("https://x.com/{username}")},
	{Slug: "discord", DisplayLabel: "Discord", ScopePolicy: ScopePolicyNone, Normalization: NormalizationLower, NormalizationVersion: 1},
	{Slug: "slack", DisplayLabel: "Slack", ScopePolicy: ScopePolicyRequired, DefaultScopeKind: new("workspace"), Normalization: NormalizationLower, NormalizationVersion: 1},
	{Slug: "linkedin", DisplayLabel: "LinkedIn", ScopePolicy: ScopePolicyNone, Normalization: NormalizationLower, NormalizationVersion: 1, ProfileURLTemplate: new("https://www.linkedin.com/in/{username}")},
	{Slug: "sms", DisplayLabel: "SMS", ScopePolicy: ScopePolicyNone, Normalization: NormalizationPhoneE164, NormalizationVersion: 1, URIScheme: new("sms")},
	{Slug: "rcs", DisplayLabel: "RCS", ScopePolicy: ScopePolicyNone, Normalization: NormalizationPhoneE164, NormalizationVersion: 1, URIScheme: new("sms")},
	{Slug: "google-messages", DisplayLabel: "Google Messages", Aliases: []string{"gmessages"}, ScopePolicy: ScopePolicyNone, Normalization: NormalizationPhoneE164, NormalizationVersion: 1, URIScheme: new("sms")},
	{Slug: "google-voice", DisplayLabel: "Google Voice", ScopePolicy: ScopePolicyNone, Normalization: NormalizationPhoneE164, NormalizationVersion: 1, URIScheme: new("tel")},
	{Slug: "google-chat", DisplayLabel: "Google Chat", ScopePolicy: ScopePolicyOptional, DefaultScopeKind: new("account"), Normalization: NormalizationEmail, NormalizationVersion: 1},
	{Slug: "irc", DisplayLabel: "IRC", ScopePolicy: ScopePolicyRequired, DefaultScopeKind: new("network"), Normalization: NormalizationLower, NormalizationVersion: 1, URIScheme: new("irc")},
	{Slug: "groupme", DisplayLabel: "GroupMe", ScopePolicy: ScopePolicyNone, Normalization: NormalizationPhoneE164, NormalizationVersion: 1},
	{Slug: "imessage", DisplayLabel: "iMessage", ScopePolicy: ScopePolicyNone, Normalization: NormalizationByAddressKind, NormalizationVersion: 1},
	{Slug: "line", DisplayLabel: "LINE", ScopePolicy: ScopePolicyNone, Normalization: NormalizationLower, NormalizationVersion: 1},
	{Slug: "bluesky", DisplayLabel: "Bluesky", Aliases: []string{"bsky"}, ScopePolicy: ScopePolicyNone, Normalization: NormalizationStripAtLower, NormalizationVersion: 1, ProfileURLTemplate: new("https://bsky.app/profile/{username}")},
	// Matrix identifiers remain case-sensitive; lowercasing could merge two
	// distinct archived participants.
	{Slug: "matrix", DisplayLabel: "Matrix", ScopePolicy: ScopePolicyRequired, DefaultScopeKind: new("server"), Normalization: NormalizationNone, NormalizationVersion: 1, URIScheme: new("matrix")},
	{Slug: "reddit", DisplayLabel: "Reddit", ScopePolicy: ScopePolicyNone, Normalization: NormalizationStripAtLower, NormalizationVersion: 1, ProfileURLTemplate: new("https://www.reddit.com/user/{username}")},
	{Slug: "kakaotalk", DisplayLabel: "KakaoTalk", ScopePolicy: ScopePolicyNone, Normalization: NormalizationLower, NormalizationVersion: 1},
	{Slug: "wechat", DisplayLabel: "WeChat", ScopePolicy: ScopePolicyNone, Normalization: NormalizationLower, NormalizationVersion: 1},
}

func (s *Store) ListCommunicationServicesContext(ctx context.Context, includeInactive bool) ([]CommunicationService, error) {
	query := `SELECT id, slug, display_label, scope_policy, default_scope_kind,
		normalization, normalization_version, uri_scheme, profile_url_template,
		is_system, is_active, created_at, updated_at
		FROM communication_services`
	if !includeInactive {
		query += ` WHERE is_active = TRUE`
	}
	query += ` ORDER BY slug`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list communication services: %w", err)
	}
	defer func() { _ = rows.Close() }()

	services := make([]CommunicationService, 0)
	for rows.Next() {
		service, err := scanCommunicationService(rows)
		if err != nil {
			return nil, fmt.Errorf("scan communication service: %w", err)
		}
		services = append(services, *service)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list communication services: %w", err)
	}
	aliases, err := s.loadAllServiceAliasesContext(ctx)
	if err != nil {
		return nil, err
	}
	for i := range services {
		services[i].Aliases = aliases[services[i].ID]
	}
	return services, nil
}

func (s *Store) GetCommunicationServiceContext(ctx context.Context, id int64) (*CommunicationService, error) {
	service, err := scanCommunicationService(s.db.QueryRowContext(ctx, serviceSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get communication service: %w", err)
	}
	service.Aliases, err = s.loadServiceAliasesContext(ctx, id)
	return service, err
}

func (s *Store) ResolveCommunicationServiceContext(ctx context.Context, slugOrAlias string) (*CommunicationService, error) {
	lookup := strings.ToLower(strings.TrimSpace(slugOrAlias))
	service, err := scanCommunicationService(s.db.QueryRowContext(ctx, serviceSelect+`
		WHERE slug = ? OR id = (
			SELECT service_id FROM communication_service_aliases WHERE alias = ?
		)
		ORDER BY CASE WHEN slug = ? THEN 0 ELSE 1 END
		LIMIT 1`, lookup, lookup, lookup))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve communication service: %w", err)
	}
	service.Aliases, err = s.loadServiceAliasesContext(ctx, service.ID)
	return service, err
}

func (s *Store) EnsureCommunicationServiceContext(ctx context.Context, input CommunicationServiceInput) (*CommunicationService, bool, error) {
	if err := validateCommunicationServiceInput(input); err != nil {
		return nil, false, err
	}
	var service *CommunicationService
	created := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockCommunicationServiceNamesTx(ctx, tx, input); err != nil {
			return err
		}
		var err error
		service, err = getCommunicationServiceBySlugTx(ctx, tx, input.Slug)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrServiceNotFound) {
			return err
		}
		names := make([]string, 0, len(input.Aliases)+1)
		names = append(names, input.Slug)
		names = append(names, input.Aliases...)
		if err := ensureAliasesAvailableTx(ctx, tx, 0, names); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO communication_services (
			slug, display_label, scope_policy, default_scope_kind, normalization,
			normalization_version, uri_scheme, profile_url_template, is_system
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, FALSE) RETURNING id`,
			input.Slug, input.DisplayLabel, input.ScopePolicy, stringValue(input.DefaultScopeKind),
			input.Normalization, input.NormalizationVersion, stringValue(input.URIScheme),
			stringValue(input.ProfileURLTemplate),
		).Scan(&id); err != nil {
			return fmt.Errorf("insert communication service: %w", err)
		}
		if err := s.replaceServiceAliasesTx(ctx, tx, id, input.Aliases); err != nil {
			return err
		}
		service, err = getCommunicationServiceTx(ctx, tx, id)
		created = err == nil
		return err
	})
	return service, created, err
}

func (s *Store) UpdateCommunicationServiceContext(ctx context.Context, id int64, input CommunicationServiceInput) (*CommunicationService, error) {
	if err := validateCommunicationServiceInput(input); err != nil {
		return nil, err
	}
	var service *CommunicationService
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockCommunicationServiceNamesTx(ctx, tx, input); err != nil {
			return err
		}
		existing, err := getCommunicationServiceTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if existing.Slug != input.Slug {
			return ErrServiceSlugConflict
		}
		if err := ensureAliasesAvailableTx(ctx, tx, id, input.Aliases); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE communication_services SET
			display_label = ?, scope_policy = ?, default_scope_kind = ?,
			normalization = ?, normalization_version = ?, uri_scheme = ?,
			profile_url_template = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ?`,
			input.DisplayLabel, input.ScopePolicy, stringValue(input.DefaultScopeKind),
			input.Normalization, input.NormalizationVersion, stringValue(input.URIScheme),
			stringValue(input.ProfileURLTemplate), id,
		); err != nil {
			return fmt.Errorf("update communication service: %w", err)
		}
		if err := s.replaceServiceAliasesTx(ctx, tx, id, input.Aliases); err != nil {
			return err
		}
		service, err = getCommunicationServiceTx(ctx, tx, id)
		return err
	})
	return service, err
}

func (s *Store) SetCommunicationServiceActiveContext(ctx context.Context, id int64, active bool) (*CommunicationService, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE communication_services
		SET is_active = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`, active, id)
	if err != nil {
		return nil, fmt.Errorf("set communication service active: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check communication service update: %w", err)
	}
	if changed == 0 {
		return nil, ErrServiceNotFound
	}
	return s.GetCommunicationServiceContext(ctx, id)
}

// NormalizeServiceValue applies the service's versioned lookup strategy.
func NormalizeServiceValue(service *CommunicationService, addressKind ContactAddressKind, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", ErrNormalizationRejected
	}
	var strategy string
	if service != nil {
		strategy = service.Normalization
	} else {
		switch addressKind {
		case ContactAddressEmail:
			strategy = NormalizationEmail
		case ContactAddressPhone:
			strategy = NormalizationPhoneE164
		case ContactAddressLanguage:
			strategy = NormalizationLower
		default:
			strategy = NormalizationNone
		}
	}
	if strategy == NormalizationByAddressKind {
		switch addressKind {
		case ContactAddressEmail:
			strategy = NormalizationEmail
		case ContactAddressPhone:
			strategy = NormalizationPhoneE164
		default:
			strategy = NormalizationNone
		}
	}
	switch strategy {
	case NormalizationNone:
		return value, nil
	case NormalizationLower, NormalizationEmail:
		return strings.ToLower(value), nil
	case NormalizationStripAtLower:
		return strings.ToLower(strings.TrimPrefix(value, "@")), nil
	case NormalizationPhoneE164:
		normalized, err := textimport.NormalizePhone(value)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrNormalizationRejected, err)
		}
		return normalized, nil
	default:
		return "", ErrInvalidNormalization
	}
}

func ValidateServiceScope(service *CommunicationService, scopeKind, scopeValue *string) error {
	hasKind := scopeKind != nil && strings.TrimSpace(*scopeKind) != ""
	hasValue := scopeValue != nil && strings.TrimSpace(*scopeValue) != ""
	if service != nil {
		switch service.ScopePolicy {
		case ScopePolicyRequired:
			if !hasKind || !hasValue {
				return ErrServiceScopeRequired
			}
		case ScopePolicyNone:
			if hasKind || hasValue {
				return ErrServiceScopeForbidden
			}
		}
	}
	// A scope kind without a value (or the reverse) would fragment identity
	// keys: the same address would land under distinct half-scoped keys.
	if hasKind != hasValue {
		return ErrServiceScopeIncomplete
	}
	return nil
}

// normalizeScopeInput trims a scope kind or value and treats blank input as
// absent, so blank-vs-NULL and padded variants cannot fragment identity keys.
func normalizeScopeInput(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func (s *Store) seedCommunicationServices(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var applied int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`,
			communicationServicesSeedV1,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check communication service seed: %w", err)
		}
		if applied > 0 {
			return nil
		}
		insert := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO communication_services (
			slug, display_label, scope_policy, default_scope_kind, normalization,
			normalization_version, uri_scheme, profile_url_template, is_system
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, TRUE)`)
		for _, input := range seededCommunicationServices {
			if _, err := tx.ExecContext(ctx, insert,
				input.Slug, input.DisplayLabel, input.ScopePolicy, stringValue(input.DefaultScopeKind),
				input.Normalization, input.NormalizationVersion, stringValue(input.URIScheme),
				stringValue(input.ProfileURLTemplate),
			); err != nil {
				return fmt.Errorf("seed communication service %q: %w", input.Slug, err)
			}
			service, err := getCommunicationServiceBySlugTx(ctx, tx, input.Slug)
			if err != nil {
				return err
			}
			for _, alias := range input.Aliases {
				if _, err := tx.ExecContext(ctx,
					s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO communication_service_aliases (alias, service_id) VALUES (?, ?)`),
					strings.ToLower(alias), service.ID,
				); err != nil {
					return fmt.Errorf("seed communication service alias %q: %w", alias, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx,
			s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO applied_migrations (name) VALUES (?)`),
			communicationServicesSeedV1,
		); err != nil {
			return fmt.Errorf("record communication service seed: %w", err)
		}
		return nil
	})
}

const serviceSelect = `SELECT id, slug, display_label, scope_policy, default_scope_kind,
	normalization, normalization_version, uri_scheme, profile_url_template,
	is_system, is_active, created_at, updated_at
	FROM communication_services`

func scanCommunicationService(row scanner) (*CommunicationService, error) {
	var service CommunicationService
	var defaultScopeKind, uriScheme, profileURLTemplate sql.NullString
	if err := row.Scan(
		&service.ID, &service.Slug, &service.DisplayLabel, &service.ScopePolicy,
		&defaultScopeKind, &service.Normalization, &service.NormalizationVersion,
		&uriScheme, &profileURLTemplate, &service.IsSystem, &service.IsActive,
		&service.CreatedAt, &service.UpdatedAt,
	); err != nil {
		return nil, err
	}
	service.DefaultScopeKind = nullStringPtr(defaultScopeKind)
	service.URIScheme = nullStringPtr(uriScheme)
	service.ProfileURLTemplate = nullStringPtr(profileURLTemplate)
	return &service, nil
}

func getCommunicationServiceTx(ctx context.Context, tx *loggedTx, id int64) (*CommunicationService, error) {
	service, err := scanCommunicationService(tx.QueryRowContext(ctx, serviceSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get communication service: %w", err)
	}
	service.Aliases, err = loadServiceAliasesTx(ctx, tx, id)
	return service, err
}

func getCommunicationServiceBySlugTx(ctx context.Context, tx *loggedTx, slug string) (*CommunicationService, error) {
	service, err := scanCommunicationService(tx.QueryRowContext(ctx, serviceSelect+` WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get communication service by slug: %w", err)
	}
	service.Aliases, err = loadServiceAliasesTx(ctx, tx, service.ID)
	return service, err
}

func (s *Store) loadServiceAliasesContext(ctx context.Context, serviceID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT alias FROM communication_service_aliases WHERE service_id = ? ORDER BY alias`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("load communication service aliases: %w", err)
	}
	return scanAliases(rows)
}

func loadServiceAliasesTx(ctx context.Context, tx *loggedTx, serviceID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT alias FROM communication_service_aliases WHERE service_id = ? ORDER BY alias`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("load communication service aliases: %w", err)
	}
	return scanAliases(rows)
}

func scanAliases(rows *loggedRows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	aliases := make([]string, 0)
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, fmt.Errorf("scan communication service alias: %w", err)
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load communication service aliases: %w", err)
	}
	return aliases, nil
}

func (s *Store) loadAllServiceAliasesContext(ctx context.Context) (map[int64][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT service_id, alias FROM communication_service_aliases ORDER BY service_id, alias`,
	)
	if err != nil {
		return nil, fmt.Errorf("load communication service aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	aliases := make(map[int64][]string)
	for rows.Next() {
		var serviceID int64
		var alias string
		if err := rows.Scan(&serviceID, &alias); err != nil {
			return nil, fmt.Errorf("scan communication service alias: %w", err)
		}
		aliases[serviceID] = append(aliases[serviceID], alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load communication service aliases: %w", err)
	}
	return aliases, nil
}

func validateCommunicationServiceInput(input CommunicationServiceInput) error {
	if !serviceSlugPattern.MatchString(input.Slug) {
		return ErrInvalidServiceSlug
	}
	switch input.ScopePolicy {
	case ScopePolicyNone, ScopePolicyOptional, ScopePolicyRequired:
	default:
		return ErrInvalidScopePolicy
	}
	switch input.Normalization {
	case NormalizationNone, NormalizationLower, NormalizationEmail,
		NormalizationPhoneE164, NormalizationStripAtLower, NormalizationByAddressKind:
	default:
		return ErrInvalidNormalization
	}
	if strings.TrimSpace(input.DisplayLabel) == "" || input.NormalizationVersion < 1 {
		return ErrInvalidNormalization
	}
	return nil
}

func ensureAliasesAvailableTx(ctx context.Context, tx *loggedTx, serviceID int64, aliases []string) error {
	for _, raw := range aliases {
		alias := strings.ToLower(strings.TrimSpace(raw))
		if alias == "" {
			return ErrServiceAliasConflict
		}
		var slugOwner int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM communication_services WHERE slug = ?`, alias,
		).Scan(&slugOwner)
		if err == nil && slugOwner != serviceID {
			return ErrServiceAliasConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check communication service alias slug: %w", err)
		}
		var aliasOwner int64
		err = tx.QueryRowContext(ctx,
			`SELECT service_id FROM communication_service_aliases WHERE alias = ?`, alias,
		).Scan(&aliasOwner)
		if err == nil && aliasOwner != serviceID {
			return ErrServiceAliasConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check communication service alias: %w", err)
		}
	}
	return nil
}

func (s *Store) replaceServiceAliasesTx(
	ctx context.Context, tx *loggedTx, serviceID int64, aliases []string,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM communication_service_aliases WHERE service_id = ?`, serviceID,
	); err != nil {
		return fmt.Errorf("replace communication service aliases: %w", err)
	}
	unique := make(map[string]struct{}, len(aliases))
	for _, raw := range aliases {
		alias := strings.ToLower(strings.TrimSpace(raw))
		unique[alias] = struct{}{}
	}
	sorted := make([]string, 0, len(unique))
	for alias := range unique {
		sorted = append(sorted, alias)
	}
	sort.Strings(sorted)
	for _, alias := range sorted {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO communication_service_aliases (alias, service_id) VALUES (?, ?)`,
			alias, serviceID,
		); err != nil {
			if s.dialect.IsConflictError(err) {
				return ErrServiceAliasConflict
			}
			return fmt.Errorf("insert communication service alias %q: %w", alias, err)
		}
	}
	return nil
}

func (s *Store) lockCommunicationServiceNamesTx(
	ctx context.Context, tx *loggedTx, input CommunicationServiceInput,
) error {
	names := make(map[string]struct{}, len(input.Aliases)+1)
	names[input.Slug] = struct{}{}
	for _, raw := range input.Aliases {
		names[strings.ToLower(strings.TrimSpace(raw))] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "communication-service-name", name,
		); err != nil {
			return err
		}
	}
	return nil
}
