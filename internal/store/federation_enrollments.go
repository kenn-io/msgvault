package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var (
	ErrNotFound             = errors.New("not found")
	ErrFederationValidation = errors.New("invalid federation enrollment")
	ErrFederationHubSource  = errors.New("federation hub cannot create sources")
	federationUIDPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	federationMemberPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

type FederationEnrollment struct {
	EnrollmentUID   string     `json:"enrollment_uid"`
	SpokeArchiveUID string     `json:"spoke_archive_uid"`
	Member          string     `json:"member"`
	SpokeName       string     `json:"spoke_name"`
	CreatedAt       time.Time  `json:"created_at"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}

const federationColumns = `enrollment_uid, spoke_archive_uid, member_handle, spoke_name, created_at, revoked_at`

func federationTokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func scanFederation(row scanner) (*FederationEnrollment, error) {
	var e FederationEnrollment
	var revoked sql.NullTime
	err := row.Scan(&e.EnrollmentUID, &e.SpokeArchiveUID, &e.Member, &e.SpokeName, &e.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.RevokedAt = activityTimePointer(revoked)
	return &e, nil
}
func (s *Store) CreateFederationEnrollmentContext(ctx context.Context, spokeUID, member, name string) (*FederationEnrollment, string, error) {
	if !federationUIDPattern.MatchString(spokeUID) {
		return nil, "", fmt.Errorf("spoke_instance must be 64 lowercase hexadecimal digits: %w", ErrFederationValidation)
	}
	if !federationMemberPattern.MatchString(member) {
		return nil, "", fmt.Errorf("member must be 1–64 lowercase ASCII letters, digits, dots, underscores or hyphens, starting with a letter or digit: %w", ErrFederationValidation)
	}
	if len(name) > 128 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, "", fmt.Errorf("spoke_name must be at most 128 bytes and contain no control characters: %w", ErrFederationValidation)
	}
	if name == "" {
		name = member
	}
	id := make([]byte, 16)
	secret := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return nil, "", fmt.Errorf("generate enrollment UID: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return nil, "", fmt.Errorf("generate enrollment token: %w", err)
	}
	token := "mvf1_" + base64.RawURLEncoding.EncodeToString(secret)
	e, err := scanFederation(s.db.QueryRowContext(ctx, `INSERT INTO federation_enrollments (enrollment_uid, spoke_archive_uid, member_handle, spoke_name, token_sha256) VALUES (?, ?, ?, ?, ?) RETURNING `+federationColumns, hex.EncodeToString(id), spokeUID, member, name, federationTokenDigest(token)))
	if err != nil {
		return nil, "", fmt.Errorf("create federation enrollment: %w", err)
	}
	return e, token, nil
}
func (s *Store) ListFederationEnrollmentsContext(ctx context.Context) ([]FederationEnrollment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+federationColumns+` FROM federation_enrollments ORDER BY created_at, enrollment_uid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	list := []FederationEnrollment{}
	for rows.Next() {
		e, err := scanFederation(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *e)
	}
	return list, rows.Err()
}
func (s *Store) AuthenticateFederationEnrollmentContext(ctx context.Context, token, spokeUID string) (*FederationEnrollment, error) {
	return scanFederation(s.db.QueryRowContext(ctx, `SELECT `+federationColumns+` FROM federation_enrollments WHERE token_sha256 = ? AND spoke_archive_uid = ? AND revoked_at IS NULL`, federationTokenDigest(token), spokeUID))
}
func (s *Store) RevokeFederationEnrollmentContext(ctx context.Context, uid string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE federation_enrollments SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP) WHERE enrollment_uid = ?`, uid)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) ConfigureFederationHubContext(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&count); err != nil {
		return fmt.Errorf("check hub sources: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("federation hub role needs a dedicated archive: it already has %d sources", count)
	}
	s.federationHub = true
	return nil
}
