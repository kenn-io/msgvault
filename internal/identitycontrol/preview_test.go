package identitycontrol

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func syntheticPreviewClaims() PreviewClaims {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	return PreviewClaims{Binding: PreviewBinding{
		PrincipalID: "synthetic-grant-id", AuthorityFingerprint: strings.Repeat("a", 64),
		Request:     PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}},
		Fingerprint: strings.Repeat("b", 64),
	}, IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
}

func TestIdentitySignedPreviewBindsEveryExplicitOperation(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, operation := range []Operation{OperationGraphLink, OperationGraphUnlink, OperationPersonLink, OperationPersonUnlink} {
		claims := syntheticPreviewClaims()
		claims.Binding.Request.Operation = operation
		if operation == OperationPersonLink || operation == OperationPersonUnlink {
			claims.Binding.Request.Target = IdentityTarget{ParticipantID: 1, PersonID: 3}
		}
		token, err := SignPreview(key, claims)
		require.NoError(t, err)
		require.NoError(t, VerifyPreview(key, token, claims.Binding, claims.IssuedAt.Add(time.Minute)))
	}
}

func TestIdentitySignedPreviewRejectsChangedAuthorityIntentOrEvidence(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	claims := syntheticPreviewClaims()
	token, err := SignPreview(key, claims)
	require.NoError(t, err)
	for _, mutate := range []func(*PreviewBinding){
		func(b *PreviewBinding) { b.PrincipalID = "another-synthetic-grant" },
		func(b *PreviewBinding) { b.AuthorityFingerprint = strings.Repeat("c", 64) },
		func(b *PreviewBinding) { b.Request.Operation = OperationGraphUnlink },
		func(b *PreviewBinding) { b.Request.Target.OtherParticipantID = 3 },
		func(b *PreviewBinding) { b.Fingerprint = strings.Repeat("c", 64) },
	} {
		changed := claims.Binding
		mutate(&changed)
		assert.ErrorIs(t, VerifyPreview(key, token, changed, claims.IssuedAt), ErrInvalidPreview)
	}
}

func TestIdentitySignedPreviewCanonicalizesOnlyGraphOrder(t *testing.T) {
	requirements := require.New(t)

	key := bytes.Repeat([]byte{7}, 32)
	claims := syntheticPreviewClaims()
	claims.Binding.Request.Target = IdentityTarget{ParticipantID: 2, OtherParticipantID: 1}
	token, err := SignPreview(key, claims)
	requirements.NoError(err)
	expected := syntheticPreviewClaims().Binding
	requirements.NoError(VerifyPreview(key, token, expected, claims.IssuedAt))
	claims.Binding.Request = PreviewRequest{Operation: OperationPersonLink, Target: IdentityTarget{ParticipantID: 1, PersonID: 2}}
	token, err = SignPreview(key, claims)
	requirements.NoError(err)
	expected = claims.Binding
	expected.Request.Target = IdentityTarget{ParticipantID: 2, PersonID: 1}
	requirements.ErrorIs(VerifyPreview(key, token, expected, claims.IssuedAt), ErrInvalidPreview)
}

func TestIdentitySignedPreviewRejectsTamperingAndExpiry(t *testing.T) {
	requirements := require.New(t)

	key := bytes.Repeat([]byte{7}, 32)
	claims := syntheticPreviewClaims()
	token, err := SignPreview(key, claims)
	requirements.NoError(err)
	for _, now := range []time.Time{claims.IssuedAt.Add(-time.Nanosecond), claims.ExpiresAt, claims.ExpiresAt.Add(time.Minute)} {
		requirements.ErrorIs(VerifyPreview(key, token, claims.Binding, now), ErrInvalidPreview)
	}
	parts := strings.Split(token, ".")
	for _, bad := range []string{"", "not-a-token", token + ".extra", "A" + parts[0][1:] + "." + parts[1], parts[0] + ".AAAA", strings.Repeat("x", 8193)} {
		requirements.ErrorIs(VerifyPreview(key, bad, claims.Binding, claims.IssuedAt), ErrInvalidPreview)
	}
	requirements.ErrorIs(VerifyPreview(bytes.Repeat([]byte{8}, 32), token, claims.Binding, claims.IssuedAt), ErrInvalidPreview)
}

func TestIdentitySignedPreviewRejectsUnboundedClaims(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, mutate := range []func(*PreviewClaims){
		func(c *PreviewClaims) { c.Binding.PrincipalID = "" },
		func(c *PreviewClaims) { c.Binding.PrincipalID = strings.Repeat("x", 257) },
		func(c *PreviewClaims) { c.Binding.AuthorityFingerprint = strings.Repeat("A", 64) },
		func(c *PreviewClaims) { c.Binding.Fingerprint = strings.Repeat("z", 64) },
		func(c *PreviewClaims) { c.Binding.Request.Target.ParticipantID = 9_007_199_254_740_992 },
		func(c *PreviewClaims) { c.ExpiresAt = c.IssuedAt },
		func(c *PreviewClaims) { c.ExpiresAt = c.IssuedAt.Add(5*time.Minute + time.Nanosecond) },
		func(c *PreviewClaims) { c.IssuedAt = time.Time{} },
	} {
		claims := syntheticPreviewClaims()
		mutate(&claims)
		_, err := SignPreview(key, claims)
		require.ErrorIs(t, err, ErrInvalidPreview)
	}
	_, err := SignPreview([]byte("short synthetic key"), syntheticPreviewClaims())
	require.ErrorIs(t, err, ErrInvalidPreview)
}
