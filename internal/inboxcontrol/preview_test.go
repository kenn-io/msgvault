package inboxcontrol

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreviewBindsPrincipalIntentAndState(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{1}, 32)
	claims := PreviewClaims{PrincipalID: "synthetic-grant", IntentHash: strings.Repeat("a", 64), StateHash: strings.Repeat("b", 64), IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	token, err := SignPreview(key, claims)
	require.NoError(t, err)
	require.NoError(t, VerifyPreview(key, token, claims.PrincipalID, claims.IntentHash, claims.StateHash, now.Add(time.Minute)))
	for _, tc := range []struct{ name, principal, intent, state string }{
		{"different principal", "another-grant", claims.IntentHash, claims.StateHash},
		{"changed intent", claims.PrincipalID, strings.Repeat("c", 64), claims.StateHash},
		{"changed state", claims.PrincipalID, claims.IntentHash, strings.Repeat("c", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.ErrorIs(t, VerifyPreview(key, token, tc.principal, tc.intent, tc.state, now), ErrInvalid)
		})
	}
	assert.ErrorIs(t, VerifyPreview(bytes.Repeat([]byte{2}, 32), token, claims.PrincipalID, claims.IntentHash, claims.StateHash, now), ErrInvalid)
}

func TestPreviewExpiryIsAuthenticated(t *testing.T) {
	requirements := require.New(t)

	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{1}, 32)
	claims := PreviewClaims{PrincipalID: "synthetic-grant", IntentHash: strings.Repeat("a", 64), StateHash: strings.Repeat("b", 64), IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	token, err := SignPreview(key, claims)
	require.NoError(t, err)
	requirements.ErrorIs(VerifyPreview(key, token, claims.PrincipalID, claims.IntentHash, claims.StateHash, now.Add(5*time.Minute)), ErrInvalid)
	requirements.ErrorIs(VerifyPreview(key, token, claims.PrincipalID, claims.IntentHash, claims.StateHash, now.Add(-time.Second)), ErrInvalid)
	claims.ExpiresAt = now.Add(5*time.Minute + time.Nanosecond)
	_, err = SignPreview(key, claims)
	requirements.ErrorIs(err, ErrInvalid)
	claims.ExpiresAt = now
	_, err = SignPreview(key, claims)
	requirements.ErrorIs(err, ErrInvalid)
}

func TestPreviewRejectsMalformedEnvelope(t *testing.T) {
	requirements := require.New(t)

	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{1}, 32)
	for _, token := range []string{"", "unsigned", "abc.def.extra", strings.Repeat("a", 16385)} {
		requirements.ErrorIs(VerifyPreview(key, token, "synthetic-grant", strings.Repeat("a", 64), strings.Repeat("b", 64), now), ErrInvalid)
	}
	claims := PreviewClaims{PrincipalID: "synthetic-grant", IntentHash: strings.Repeat("a", 64), StateHash: strings.Repeat("b", 64), IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	_, err := SignPreview([]byte("short"), claims)
	requirements.ErrorIs(err, ErrInvalid)
	claims.IntentHash = "not-a-digest"
	_, err = SignPreview(key, claims)
	requirements.ErrorIs(err, ErrInvalid)
	claims.IntentHash = strings.Repeat("a", 64)
	claims.PrincipalID = ""
	_, err = SignPreview(key, claims)
	requirements.ErrorIs(err, ErrInvalid)
}

func TestPreviewCannotExtendExpiryByEditingPayload(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{1}, 32)
	claims := PreviewClaims{PrincipalID: "synthetic-grant", IntentHash: strings.Repeat("a", 64), StateHash: strings.Repeat("b", 64), IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	token, err := SignPreview(key, claims)
	require.NoError(t, err)
	parts := strings.Split(token, ".")
	claims.ExpiresAt = now.Add(time.Hour)
	payload, err := json.Marshal(claims, json.Deterministic(true))
	require.NoError(t, err)
	tampered := base64.RawURLEncoding.EncodeToString(payload) + "." + parts[1]
	assert.ErrorIs(t, VerifyPreview(key, tampered, claims.PrincipalID, claims.IntentHash, claims.StateHash, now.Add(10*time.Minute)), ErrInvalid)
}
