package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// PersonInferenceConsent is one preserved grant and its optional revocation.
type PersonInferenceConsent struct {
	ID                 int64      `json:"id"`
	ProfileFingerprint string     `json:"profile_fingerprint"`
	GrantedBy          string     `json:"granted_by"`
	GrantedAt          time.Time  `json:"granted_at"`
	RevokedBy          *string    `json:"revoked_by,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
}

// PersonInferenceConsentStatus reports authority for one exact runtime
// fingerprint without exposing any credential value.
type PersonInferenceConsentStatus struct {
	Fingerprint   string                  `json:"fingerprint"`
	ProfileExists bool                    `json:"profile_exists"`
	Active        bool                    `json:"active"`
	Consent       *PersonInferenceConsent `json:"consent,omitempty"`
	LastRevoked   *PersonInferenceConsent `json:"last_revoked,omitempty"`
}

const personInferenceConsentColumns = `
	id, profile_fingerprint, granted_by, granted_at, revoked_by, revoked_at`

type personInferenceProfileProjection struct {
	Fingerprint           string         `db:"fingerprint" profile:"fingerprint"`
	Protocol              string         `db:"provider_kind" profile:"protocol"`
	Endpoint              string         `db:"endpoint" profile:"endpoint"`
	Model                 string         `db:"model" profile:"model"`
	APIKeyEnv             string         `db:"api_key_env" profile:"credential_ref"`
	AllowAnonymous        bool           `db:"allow_anonymous" profile:"auth,anonymous"`
	Auth                  string         `db:"auth_scheme" profile:"auth"`
	Credential            string         `db:"credential_source" profile:"credential"`
	CredentialRef         string         `db:"credential_ref" profile:"credential_ref"`
	OutputMode            string         `db:"output_mode" profile:"output_mode"`
	TokenLimit            string         `db:"token_limit_parameter" profile:"token_limit_parameter"`
	ReasoningEffort       string         `db:"reasoning_effort" profile:"reasoning_effort"`
	ReasoningMode         string         `db:"reasoning_mode" profile:"reasoning_mode"`
	DriverVersion         string         `db:"driver_version" profile:"driver_version"`
	Retention             string         `db:"retention_posture" profile:"retention_posture"`
	Training              string         `db:"training_posture" profile:"training_posture"`
	AllowedSources        string         `db:"allowed_sources,json" profile:"allowed_sources,json"`
	SourceSince           string         `db:"source_since" profile:"source_since"`
	SourceUntil           sql.NullString `db:"source_until" profile:"source_until,null"`
	AllowSensitive        bool           `db:"allow_sensitive" profile:"allow_sensitive"`
	ExecutionBoundary     string         `db:"execution_boundary" profile:"execution_boundary"`
	PacketRendererPolicy  string         `db:"packet_renderer_policy" profile:"packet_renderer_policy"`
	ProgramFingerprint    string         `db:"program_fingerprint" profile:"program_fingerprint"`
	DisclosedPacketFields string         `db:"disclosed_packet_fields,json" profile:"disclosed_packet_fields,json"`
	PolicyJSON            string         `db:"policy_json,json" profile:"policy_json,raw"`
}

var personInferenceProfileColumns = personInferenceProfileSelectColumns()

func personInferenceProfileSelectColumns() string {
	return personInferenceProfileColumnsFor(func(name string, isJSON bool) string {
		if isJSON {
			return "CAST(" + name + " AS TEXT)"
		}
		return name
	})
}

func personInferenceProfileInsertColumns() string {
	return personInferenceProfileColumnsFor(func(name string, _ bool) string { return name })
}

func personInferenceProfileInsertValues(jsonBindExpr string) string {
	return personInferenceProfileColumnsFor(func(_ string, isJSON bool) string {
		if isJSON {
			return jsonBindExpr
		}
		return "?"
	})
}

func personInferenceProfileColumnsFor(render func(name string, isJSON bool) string) string {
	typeOfProjection := reflect.TypeFor[personInferenceProfileProjection]()
	indexes := personInferenceProfileDBFieldIndexes(typeOfProjection)
	columns := make([]string, len(indexes))
	for columnIndex, fieldIndex := range indexes {
		field := typeOfProjection.Field(fieldIndex)
		tag := field.Tag.Get("db")
		name, options, _ := strings.Cut(tag, ",")
		columns[columnIndex] = render(name, options == "json")
	}
	return strings.Join(columns, ", ")
}

func personInferenceProfileDBFieldIndexes(typeOfProjection reflect.Type) []int {
	indexes := make([]int, 0, typeOfProjection.NumField())
	for index := range typeOfProjection.NumField() {
		name, _, _ := strings.Cut(typeOfProjection.Field(index).Tag.Get("db"), ",")
		if name != "" && name != "-" {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func newPersonInferenceProfileProjection(
	profile peoplesweep.ProviderProfile,
) (personInferenceProfileProjection, error) {
	profileValue := reflect.ValueOf(profile)
	profileFields := make(map[string]reflect.Value, profileValue.NumField())
	for index := range profileValue.NumField() {
		field := reflect.TypeOf(profile).Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.Name == "PolicyJSON" {
			profileFields["policy_json"] = profileValue.Field(index)
		} else if name != "" && name != "-" {
			profileFields[name] = profileValue.Field(index)
		}
	}

	projection := personInferenceProfileProjection{}
	projectionValue := reflect.ValueOf(&projection).Elem()
	projectionType := projectionValue.Type()
	for index := range projectionValue.NumField() {
		field := projectionType.Field(index)
		name, option, _ := strings.Cut(field.Tag.Get("profile"), ",")
		if name == "" || name == "-" {
			return personInferenceProfileProjection{}, fmt.Errorf(
				"people inference profile projection field %q has no profile tag", field.Name)
		}
		source, ok := profileFields[name]
		if !ok {
			return personInferenceProfileProjection{}, fmt.Errorf(
				"people inference profile projection field %q maps to unknown profile field %q", field.Name, name)
		}
		target := projectionValue.Field(index)
		switch option {
		case "anonymous":
			target.SetBool(source.String() == string(peoplesweep.AuthNone))
		case "json":
			encoded, err := json.Marshal(source.Interface())
			if err != nil {
				return personInferenceProfileProjection{}, fmt.Errorf(
					"encode people inference %s: %w", name, err)
			}
			target.SetString(string(encoded))
		case "null":
			target.Set(reflect.ValueOf(sql.NullString{
				String: source.String(), Valid: source.String() != "",
			}))
		case "raw":
			target.SetString(string(source.Bytes()))
		default:
			if target.Kind() == reflect.Bool && source.Kind() == reflect.Bool {
				target.SetBool(source.Bool())
				continue
			}
			if target.Kind() != reflect.String || source.Kind() != reflect.String {
				return personInferenceProfileProjection{}, fmt.Errorf(
					"people inference profile projection field %q has incompatible profile field %q", field.Name, name)
			}
			target.SetString(source.String())
		}
	}
	return projection, nil
}

func (p *personInferenceProfileProjection) scanDestinations() []any {
	value := reflect.ValueOf(p).Elem()
	indexes := personInferenceProfileDBFieldIndexes(value.Type())
	destinations := make([]any, len(indexes))
	for destinationIndex, fieldIndex := range indexes {
		destinations[destinationIndex] = value.Field(fieldIndex).Addr().Interface()
	}
	return destinations
}

func (p *personInferenceProfileProjection) insertValues() []any {
	value := reflect.ValueOf(p).Elem()
	indexes := personInferenceProfileDBFieldIndexes(value.Type())
	values := make([]any, len(indexes))
	for valueIndex, fieldIndex := range indexes {
		field := value.Field(fieldIndex)
		if field.Type() == reflect.TypeFor[sql.NullString]() {
			sourceUntil, ok := reflect.TypeAssert[sql.NullString](field)
			if !ok {
				values[valueIndex] = field.Interface()
				continue
			}
			if !sourceUntil.Valid {
				values[valueIndex] = nil
				continue
			}
			values[valueIndex] = sourceUntil.String
			continue
		}
		values[valueIndex] = field.Interface()
	}
	return values
}

func (p *personInferenceProfileProjection) equal(expected personInferenceProfileProjection) bool {
	value, expectedValue := reflect.ValueOf(p).Elem(), reflect.ValueOf(expected)
	indexes := personInferenceProfileDBFieldIndexes(value.Type())
	for _, fieldIndex := range indexes {
		field := value.Type().Field(fieldIndex)
		actual, want := value.Field(fieldIndex).Interface(), expectedValue.Field(fieldIndex).Interface()
		if field.Type == reflect.TypeFor[sql.NullString]() {
			actualUntil, actualOK := actual.(sql.NullString)
			expectedUntil, expectedOK := want.(sql.NullString)
			if !actualOK || !expectedOK {
				return false
			}
			if actualUntil.String != expectedUntil.String {
				return false
			}
			continue
		}
		_, options, _ := strings.Cut(field.Tag.Get("db"), ",")
		if options == "json" {
			actualString, actualOK := actual.(string)
			wantString, wantOK := want.(string)
			if !actualOK || !wantOK || !equalJSON([]byte(actualString), []byte(wantString)) {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(actual, want) {
			return false
		}
	}
	return true
}

func (p *personInferenceProfileProjection) profile() (peoplesweep.ProviderProfile, error) {
	var storedSources []peoplesweep.SourceClass
	if err := json.Unmarshal([]byte(p.AllowedSources), &storedSources); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode allowed sources: %w", err)
	}
	var storedDisclosedFields []string
	if err := json.Unmarshal([]byte(p.DisclosedPacketFields), &storedDisclosedFields); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode disclosed packet fields: %w", err)
	}
	var profile peoplesweep.ProviderProfile
	if err := json.Unmarshal([]byte(p.PolicyJSON), &profile); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode people inference policy: %w", err)
	}
	if profile.Protocol == "" {
		return peoplesweep.ProviderProfile{}, errors.New(
			"stored people inference profile policy has no protocol")
	}
	profile.Fingerprint = p.Fingerprint
	profile.PolicyJSON = json.RawMessage(p.PolicyJSON)
	expected, err := newPersonInferenceProfileProjection(profile)
	if err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	if !p.equal(expected) {
		return peoplesweep.ProviderProfile{}, errors.New(
			"stored people inference profile does not match its immutable policy")
	}
	canonical, err := peoplesweep.CanonicalProviderProfile(profile)
	if err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	if profile.Fingerprint != canonical.Fingerprint ||
		!equalJSON(profile.PolicyJSON, canonical.PolicyJSON) {
		return peoplesweep.ProviderProfile{}, errors.New(
			"stored people inference profile does not match its immutable policy")
	}
	return canonical, nil
}

// EnsurePersonInferenceProfile persists one immutable canonical policy or
// verifies the already-stored row has the same content.
func (s *Store) EnsurePersonInferenceProfile(
	ctx context.Context,
	profile peoplesweep.ProviderProfile,
) (bool, error) {
	if err := profile.Validate(); err != nil {
		return false, err
	}
	projection, err := newPersonInferenceProfileProjection(profile)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO person_inference_profiles
			(`+personInferenceProfileInsertColumns()+`)
		VALUES (`+personInferenceProfileInsertValues(s.dialect.JSONBindExpr())+`)
		ON CONFLICT (fingerprint) DO NOTHING`, projection.insertValues()...)
	if err != nil {
		return false, fmt.Errorf("insert people inference profile: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read people inference profile insert result: %w", err)
	}
	if err := s.verifyPersonInferenceProfile(ctx, profile, projection); err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) verifyPersonInferenceProfile(
	ctx context.Context,
	profile peoplesweep.ProviderProfile,
	expected personInferenceProfileProjection,
) error {
	var stored personInferenceProfileProjection
	err := s.db.QueryRowContext(ctx, `
		SELECT `+personInferenceProfileColumns+`
		FROM person_inference_profiles WHERE fingerprint = ?`, profile.Fingerprint).Scan(stored.scanDestinations()...)
	if err != nil {
		return fmt.Errorf("read people inference profile: %w", err)
	}
	if !stored.equal(expected) {
		return errors.New("people inference profile fingerprint already has different immutable policy")
	}
	return nil
}

// ListPersonInferenceProfiles returns every immutable policy that has been
// persisted for consent. It does not depend on the current runtime policy, so
// operators can audit and revoke old grants after configuration changes.
func (s *Store) ListPersonInferenceProfiles(
	ctx context.Context,
) ([]peoplesweep.ProviderProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+personInferenceProfileColumns+`
		FROM person_inference_profiles
		ORDER BY fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("list people inference profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	profiles := make([]peoplesweep.ProviderProfile, 0)
	for rows.Next() {
		profile, scanErr := scanPersonInferenceProfile(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("read people inference profile: %w", scanErr)
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list people inference profiles: %w", err)
	}
	return profiles, nil
}

// GrantPersonInferenceConsent grants one exact existing profile. An already
// active grant is returned as an idempotent success.
func (s *Store) GrantPersonInferenceConsent(
	ctx context.Context,
	fingerprint, actor string,
) (*PersonInferenceConsent, bool, error) {
	actor, err := validatePersonInferenceConsentInput(fingerprint, actor)
	if err != nil {
		return nil, false, err
	}
	var profileExists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM person_inference_profiles WHERE fingerprint = ?)`,
		fingerprint,
	).Scan(&profileExists); err != nil {
		return nil, false, fmt.Errorf("check people inference profile: %w", err)
	}
	if !profileExists {
		return nil, false, errors.New("people inference consent profile does not exist")
	}

	for range 3 {
		consent, insertErr := scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
			INSERT INTO person_inference_consents
				(profile_fingerprint, granted_by)
			VALUES (?, ?)
			ON CONFLICT DO NOTHING
			RETURNING `+personInferenceConsentColumns,
			fingerprint, actor,
		))
		if insertErr == nil {
			return consent, true, nil
		}
		if !errors.Is(insertErr, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("grant people inference consent: %w", insertErr)
		}
		consent, readErr := s.activePersonInferenceConsent(ctx, fingerprint)
		if readErr == nil {
			return consent, false, nil
		}
		if !errors.Is(readErr, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("read active people inference consent: %w", readErr)
		}
	}
	return nil, false, errors.New("people inference consent changed concurrently; retry")
}

// RevokePersonInferenceConsent stamps the current exact grant. Missing or
// already-revoked consent is an idempotent no-op.
func (s *Store) RevokePersonInferenceConsent(
	ctx context.Context,
	fingerprint, actor string,
) (bool, error) {
	actor, err := validatePersonInferenceConsentInput(fingerprint, actor)
	if err != nil {
		return false, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
		UPDATE person_inference_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		RETURNING id`, actor, fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revoke people inference consent: %w", err)
	}
	return true, nil
}

// RevokeAllPersonInferenceConsents stamps every active grant, including grants
// for policies that are no longer present in the runtime configuration.
func (s *Store) RevokeAllPersonInferenceConsents(
	ctx context.Context,
	actor string,
) (int64, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return 0, errors.New("people inference consent actor is required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE person_inference_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE revoked_at IS NULL`, actor)
	if err != nil {
		return 0, fmt.Errorf("revoke all people inference consents: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read revoked people inference consent count: %w", err)
	}
	return changed, nil
}

// HasActivePersonInferenceConsent implements the runner's narrow privacy gate.
func (s *Store) HasActivePersonInferenceConsent(
	ctx context.Context,
	fingerprint string,
) (bool, error) {
	if !validLowerSHA256(fingerprint) {
		return false, errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM person_inference_consents
			WHERE profile_fingerprint = ? AND revoked_at IS NULL
		)`, fingerprint).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check active people inference consent: %w", err)
	}
	return active, nil
}

func (s *Store) hasActivePersonInferenceConsentTx(
	ctx context.Context, tx *loggedTx, fingerprint string,
) (bool, error) {
	if !validLowerSHA256(fingerprint) {
		return false, errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM person_inference_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`+s.dialect.SelectForUpdate(), fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check active people inference consent in transaction: %w", err)
	}
	return id > 0, nil
}

// GetPersonInferenceConsentStatus reports exact current and historical state.
func (s *Store) GetPersonInferenceConsentStatus(
	ctx context.Context,
	fingerprint string,
) (*PersonInferenceConsentStatus, error) {
	if !validLowerSHA256(fingerprint) {
		return nil, errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	status := &PersonInferenceConsentStatus{Fingerprint: fingerprint}
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM person_inference_profiles WHERE fingerprint = ?)`,
		fingerprint,
	).Scan(&status.ProfileExists); err != nil {
		return nil, fmt.Errorf("check people inference profile status: %w", err)
	}
	if !status.ProfileExists {
		return status, nil
	}
	active, err := s.activePersonInferenceConsent(ctx, fingerprint)
	if err == nil {
		status.Active = true
		status.Consent = active
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read active people inference consent status: %w", err)
	}
	lastRevoked, err := scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personInferenceConsentColumns+`
		FROM person_inference_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NOT NULL
		ORDER BY revoked_at DESC, id DESC LIMIT 1`, fingerprint))
	if err == nil {
		status.LastRevoked = lastRevoked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read revoked people inference consent status: %w", err)
	}
	return status, nil
}

func (s *Store) activePersonInferenceConsent(
	ctx context.Context,
	fingerprint string,
) (*PersonInferenceConsent, error) {
	return scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personInferenceConsentColumns+`
		FROM person_inference_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`, fingerprint))
}

func scanPersonInferenceConsent(row scanner) (*PersonInferenceConsent, error) {
	var (
		consent              PersonInferenceConsent
		grantedAt, revokedAt nullableTimestamp
		revokedBy            sql.NullString
	)
	if err := row.Scan(
		&consent.ID, &consent.ProfileFingerprint, &consent.GrantedBy,
		&grantedAt, &revokedBy, &revokedAt,
	); err != nil {
		return nil, err
	}
	if !grantedAt.Valid {
		return nil, errors.New("people inference consent has invalid granted_at")
	}
	consent.GrantedAt = grantedAt.Time
	if revokedBy.Valid {
		value := revokedBy.String
		consent.RevokedBy = &value
	}
	consent.RevokedAt = optionalTimestamp(revokedAt)
	return &consent, nil
}

func scanPersonInferenceProfile(row scanner) (peoplesweep.ProviderProfile, error) {
	var projection personInferenceProfileProjection
	if err := row.Scan(projection.scanDestinations()...); err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	return projection.profile()
}

func validatePersonInferenceConsentInput(fingerprint, actor string) (string, error) {
	if !validLowerSHA256(fingerprint) {
		return "", errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", errors.New("people inference consent actor is required")
	}
	return actor, nil
}
