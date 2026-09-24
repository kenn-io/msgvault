package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personmatch"
)

var personMatchFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PersonMatchConsent is an audit record for one exact Jev disclosure.
type PersonMatchConsent struct {
	ID                    int64                  `json:"id"`
	DisclosureFingerprint string                 `json:"disclosure_fingerprint"`
	Disclosure            personmatch.Disclosure `json:"disclosure"`
	GrantedBy             string                 `json:"granted_by"`
	GrantedAt             time.Time              `json:"granted_at"`
}

// GrantPersonMatchConsentContext records explicit consent for one immutable
// disclosure. Repeated grants while active return the original row.
func (s *Store) GrantPersonMatchConsentContext(ctx context.Context, disclosure personmatch.Disclosure, actor string) (*PersonMatchConsent, bool, error) {
	fingerprint, err := disclosure.Fingerprint()
	if err != nil {
		return nil, false, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, false, errors.New("person match consent actor is required")
	}
	result, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		INSERT INTO person_match_consents
			(disclosure_fingerprint, endpoint, model_id, packet_schema,
			 retention_declaration, policy_version, question_version, granted_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (disclosure_fingerprint) WHERE revoked_at IS NULL DO NOTHING`),
		fingerprint, disclosure.Endpoint, disclosure.ModelID, disclosure.PacketSchema,
		disclosure.RetentionDeclaration, disclosure.PolicyVersion, disclosure.QuestionVersion, actor)
	if err != nil {
		return nil, false, fmt.Errorf("grant person match consent: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("read person match consent insert result: %w", err)
	}
	consent := &PersonMatchConsent{}
	err = s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT id, disclosure_fingerprint, endpoint, model_id, packet_schema,
		       retention_declaration, policy_version, question_version, granted_by, granted_at
		FROM person_match_consents
		WHERE disclosure_fingerprint = ? AND revoked_at IS NULL`), fingerprint).Scan(
		&consent.ID, &consent.DisclosureFingerprint, &consent.Disclosure.Endpoint,
		&consent.Disclosure.ModelID, &consent.Disclosure.PacketSchema,
		&consent.Disclosure.RetentionDeclaration, &consent.Disclosure.PolicyVersion,
		&consent.Disclosure.QuestionVersion, &consent.GrantedBy, &consent.GrantedAt)
	if err != nil {
		return nil, false, fmt.Errorf("read person match consent: %w", err)
	}
	verified, err := consent.Disclosure.Fingerprint()
	if err != nil || verified != fingerprint || consent.Disclosure != disclosure {
		return nil, false, errors.New("person match consent disclosure fingerprint conflicts with stored policy")
	}
	return consent, rows == 1, nil
}

// RevokePersonMatchConsentContext closes the active grant while retaining its
// audit row. A repeated revocation reports no change.
func (s *Store) RevokePersonMatchConsentContext(ctx context.Context, fingerprint, actor string) (bool, error) {
	if !personMatchFingerprint.MatchString(fingerprint) {
		return false, errors.New("person match disclosure fingerprint is invalid")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return false, errors.New("person match consent actor is required")
	}
	result, err := s.db.ExecContext(ctx, s.dialect.Rebind(`
		UPDATE person_match_consents SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE disclosure_fingerprint = ? AND revoked_at IS NULL`), actor, fingerprint)
	if err != nil {
		return false, fmt.Errorf("revoke person match consent: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read person match consent revocation result: %w", err)
	}
	return rows == 1, nil
}

// HasPersonMatchConsentContext is the exact disclosure egress gate.
func (s *Store) HasPersonMatchConsentContext(ctx context.Context, fingerprint string) (bool, error) {
	if !personMatchFingerprint.MatchString(fingerprint) {
		return false, errors.New("person match disclosure fingerprint is invalid")
	}
	var active bool
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT EXISTS (SELECT 1 FROM person_match_consents
		WHERE disclosure_fingerprint = ? AND revoked_at IS NULL)`), fingerprint).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check person match consent: %w", err)
	}
	return active, nil
}
