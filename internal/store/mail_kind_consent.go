package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/mailkind"
)

func (s *Store) ensureMailKindProfile(ctx context.Context, disclosure mailkind.Disclosure) (string, error) {
	fingerprint, err := disclosure.Fingerprint()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(disclosure, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mail_kind_profiles
		(fingerprint, provider, endpoint, model, policy_json)
		VALUES (?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`) ON CONFLICT (fingerprint) DO NOTHING`,
		fingerprint, disclosure.Provider, disclosure.Endpoint, disclosure.ModelID, string(encoded))
	if err != nil {
		return "", fmt.Errorf("persist mail kind disclosure: %w", err)
	}
	if err := s.verifyMailKindProfile(ctx, fingerprint); err != nil {
		return "", err
	}
	return fingerprint, nil
}

func (s *Store) verifyMailKindProfile(ctx context.Context, fingerprint string) error {
	var provider, endpoint, model, policy string
	err := s.db.QueryRowContext(ctx, `SELECT provider, endpoint, model, CAST(policy_json AS TEXT)
		FROM mail_kind_profiles WHERE fingerprint = ?`, fingerprint).Scan(&provider, &endpoint, &model, &policy)
	if err != nil {
		return fmt.Errorf("read mail kind disclosure: %w", err)
	}
	var disclosure mailkind.Disclosure
	if err := json.Unmarshal([]byte(policy), &disclosure); err != nil {
		return fmt.Errorf("decode mail kind disclosure: %w", err)
	}
	actual, err := disclosure.Fingerprint()
	if err != nil || actual != fingerprint || provider != disclosure.Provider ||
		endpoint != disclosure.Endpoint || model != disclosure.ModelID {
		return errors.New("mail kind profile conflicts with its immutable disclosure")
	}
	return nil
}

// GrantMailKindConsentContext grants only the exact mail-specific disclosure.
// Other purposes never authorize this one. Consent changes share the daemon's
// request lock, so revoke cannot finish while a provider request is in flight.
func (s *Store) GrantMailKindConsentContext(ctx context.Context, disclosure mailkind.Disclosure, actor string, mutationGate func(context.Context) (func(), error)) (*ProviderConsent, bool, error) {
	mu := &s.withoutSyncScope().mailKindConsentMu
	mu.Lock()
	defer mu.Unlock()
	if mutationGate != nil {
		release, err := mutationGate(ctx)
		if err != nil {
			return nil, false, err
		}
		defer release()
	}
	fingerprint, err := s.ensureMailKindProfile(ctx, disclosure)
	if err != nil {
		return nil, false, err
	}
	return s.grantConsent(ctx, ConsentMailKindDecision, fingerprint, actor)
}

func (s *Store) RevokeMailKindConsentContext(ctx context.Context, fingerprint, actor string, mutationGate func(context.Context) (func(), error)) (bool, error) {
	mu := &s.withoutSyncScope().mailKindConsentMu
	mu.Lock()
	defer mu.Unlock()
	if mutationGate != nil {
		release, err := mutationGate(ctx)
		if err != nil {
			return false, err
		}
		defer release()
	}
	return revokeConsent(ctx, s.db, ConsentMailKindDecision, fingerprint, actor)
}

func (s *Store) HasMailKindConsentContext(ctx context.Context, fingerprint string) (bool, error) {
	if err := validateConsentFingerprint(ConsentMailKindDecision, fingerprint); err != nil {
		return false, err
	}
	if err := s.verifyMailKindProfile(ctx, fingerprint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return s.hasActiveConsent(ctx, ConsentMailKindDecision, fingerprint)
}

// MailKindConsentEgressContext must guard every attempt, including retries.
// It does not hold an archive transaction while the provider is called.
func (s *Store) MailKindConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error) {
	if dispatch == nil {
		return false, errors.New("mail kind provider dispatch is required")
	}
	mu := &s.withoutSyncScope().mailKindConsentMu
	mu.RLock()
	defer mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	active, err := s.HasMailKindConsentContext(ctx, fingerprint)
	if err != nil || !active {
		return false, err
	}
	return true, dispatch()
}
