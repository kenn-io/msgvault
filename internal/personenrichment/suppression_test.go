package personenrichment_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

const suppressionTestDomain = "msgvault/person-enrichment/suppression/v1"

func TestSuppressionDigestIsProviderAndClassScoped(t *testing.T) {
	checks := assert.New(t)
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x31}, 32))
	require.NoError(t, err)
	exa := hasher.Digest(
		"exa:"+strings.Repeat("a", 64),
		personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1,
		"person@example.com",
	)
	otherProvider := hasher.Digest(
		"sixtyfour:"+strings.Repeat("b", 64),
		personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1,
		"person@example.com",
	)
	otherClass := hasher.Digest(
		exa.ProviderNamespace,
		personenrichment.SuppressionPhone,
		personenrichment.PhoneNormalizationV1,
		"person@example.com",
	)
	checks.NotEqual(exa.Digest, otherProvider.Digest)
	checks.NotEqual(exa.Digest, otherClass.Digest)
	checks.Equal(exa.KeyID, otherProvider.KeyID)
	checks.Equal(exa.KeyID, otherClass.KeyID)
}

func TestSuppressionDigestUsesVersionedHMACFormatAndCopiesKey(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	key := bytes.Repeat([]byte{0x42}, 32)
	originalKey := slices.Clone(key)
	hasher, err := personenrichment.NewSuppressionHasher(key)
	requirements.NoError(err)
	clear(key)

	namespace := "exa:" + strings.Repeat("c", 64)
	got := hasher.Digest(
		namespace,
		personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1,
		"person@example.com",
	)

	keyIDInput := suppressionTestDomain + "\x00key-id\x00" + string(originalKey)
	wantKeyID := sha256.Sum256([]byte(keyIDInput))
	wantDigest := hmac.New(sha256.New, originalKey)
	_, err = wantDigest.Write([]byte(
		suppressionTestDomain + "\x00" + namespace + "\x00email\x00email-v1\x00person@example.com",
	))
	requirements.NoError(err)
	checks.Equal(hex.EncodeToString(wantKeyID[:]), got.KeyID)
	checks.Equal(wantDigest.Sum(nil), got.Digest)
	checks.Equal(namespace, got.ProviderNamespace)
	checks.Equal(personenrichment.SuppressionEmail, got.IdentifierClass)
	checks.Equal(personenrichment.EmailNormalizationV1, got.NormalizationVersion)
}

func TestSuppressionHasherRejectsShortKeysWithoutExposingThem(t *testing.T) {
	secret := "short-secret"
	_, err := personenrichment.NewSuppressionHasher([]byte(secret))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)

	_, err = personenrichment.NewSuppressionHasher(nil)
	require.Error(t, err)
}

func TestZeroValueSuppressionHasherCannotProduceDigest(t *testing.T) {
	assert.NotPanics(t, func() {
		got := (&personenrichment.SuppressionHasher{}).Digest(
			"exa:"+strings.Repeat("a", 64),
			personenrichment.SuppressionEmail,
			personenrichment.EmailNormalizationV1,
			"person@example.com",
		)
		assert.Equal(t, personenrichment.SuppressionDigest{}, got)
	})
}

func TestNewClaimCommitVerifiesReturnedIdentifierManifest(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	key := bytes.Repeat([]byte{0x55}, 32)
	hasher, err := personenrichment.NewSuppressionHasher(key)
	requirements.NoError(err)
	namespace := "exa:" + strings.Repeat("d", 64)
	result := claimCommitResult()
	input := validClaimCommitInput(namespace)

	commit, err := personenrichment.NewClaimCommit(input, result, hasher)
	requirements.NoError(err)
	digests, err := commit.VerifiedReturnedIdentifierDigests()
	requirements.NoError(err)
	requirements.Len(digests, 4)

	want := []personenrichment.SuppressionDigest{
		expectedSuppressionDigest(key, namespace, personenrichment.SuppressionProviderPersonID,
			personenrichment.ProviderPersonIDNormalizationV1, "CaseSensitive-ID"),
		expectedSuppressionDigest(key, namespace, personenrichment.SuppressionProviderPersonID,
			personenrichment.ProviderPersonIDNormalizationV1, "caseSensitive-ID"),
		expectedSuppressionDigest(key, namespace, personenrichment.SuppressionPublicProfileURL,
			personenrichment.URLNormalizationV1, "https://example.com/people/a"),
		expectedSuppressionDigest(key, namespace, personenrichment.SuppressionPublicProfileURL,
			personenrichment.URLNormalizationV1, "https://example.com/people/b"),
	}
	slices.SortFunc(want, compareSuppressionDigests)
	checks.Equal(want, digests)

	safeManifest := fmt.Sprint(digests)
	checks.NotContains(safeManifest, "CaseSensitive-ID")
	checks.NotContains(safeManifest, "caseSensitive-ID")
	checks.NotContains(safeManifest, "example.com")
}

func TestNewClaimCommitDefensivelyCopiesResultAndDigestBytes(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x61}, 32))
	requirements.NoError(err)
	result := claimCommitResult()
	commit, err := personenrichment.NewClaimCommit(
		validClaimCommitInput("exa:"+strings.Repeat("e", 64)), result, hasher,
	)
	requirements.NoError(err)

	result.ProviderPersonIDs[0].ID = "mutated-provider-id"
	result.CanonicalPublicURLs[0] = "https://mutated.example.test/"
	result.Citations[0].Title = "mutated title"
	result.Claims[0].SubmittedValue[0] = 'X'
	result.Claims[0].Evidence[0].Excerpt = "mutated excerpt"
	*result.Claims[0].Evidence[0].SubjectPersonID = 999

	got := commit.Result()
	checks.Equal("CaseSensitive-ID", got.ProviderPersonIDs[0].ID)
	checks.Equal("https://example.com/people/a", got.CanonicalPublicURLs[0])
	checks.Equal("original title", got.Citations[0].Title)
	checks.JSONEq(`"original"`, string(got.Claims[0].SubmittedValue))
	checks.Equal("original excerpt", got.Claims[0].Evidence[0].Excerpt)
	checks.EqualValues(7, *got.Claims[0].Evidence[0].SubjectPersonID)
	checks.Empty(got.IdentityMatches, "transient returned identity values must not reach the sink")

	got.ProviderPersonIDs[0].ID = "accessor mutation"
	got.Claims[0].SubmittedValue[0] = 'Y'
	again := commit.Result()
	checks.Equal("CaseSensitive-ID", again.ProviderPersonIDs[0].ID)
	checks.JSONEq(`"original"`, string(again.Claims[0].SubmittedValue))

	digests, err := commit.VerifiedReturnedIdentifierDigests()
	requirements.NoError(err)
	originalDigest := slices.Clone(digests[0].Digest)
	clear(digests[0].Digest)
	againDigests, err := commit.VerifiedReturnedIdentifierDigests()
	requirements.NoError(err)
	checks.Equal(originalDigest, againDigests[0].Digest)
}

func TestNewClaimCommitDeepCopyPreservesResultOrdering(t *testing.T) {
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x66}, 32))
	require.NoError(t, err)
	result := claimCommitResult()
	result.Claims[0].Target = personfacts.TargetDescriptor{
		Choices: []personfacts.ChoiceDescriptor{{Value: "z"}, {Value: "a"}},
		Fields:  []personfacts.FieldDescriptor{{Name: "z"}, {Name: "a"}},
	}
	want := result
	want.IdentityMatches = nil
	commit, err := personenrichment.NewClaimCommit(
		validClaimCommitInput("exa:"+strings.Repeat("9", 64)), result, hasher,
	)
	require.NoError(t, err)
	assert.Equal(t, want, commit.Result())
}

func TestNewClaimCommitRejectsUnverifiableReturnedIdentifiers(t *testing.T) {
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x71}, 32))
	require.NoError(t, err)
	validNamespace := "exa:" + strings.Repeat("f", 64)
	tests := []struct {
		name      string
		namespace string
		mutate    func(*personenrichment.Result)
	}{
		{"duplicate provider ID", validNamespace, func(r *personenrichment.Result) {
			r.ProviderPersonIDs[1].ID = " CaseSensitive-ID "
		}},
		{"empty provider ID", validNamespace, func(r *personenrichment.Result) {
			r.ProviderPersonIDs[0].ID = " "
		}},
		{"duplicate public URL", validNamespace, func(r *personenrichment.Result) {
			r.CanonicalPublicURLs[1] = r.CanonicalPublicURLs[0]
		}},
		{"noncanonical public URL", validNamespace, func(r *personenrichment.Result) {
			r.CanonicalPublicURLs[0] = "HTTPS://EXAMPLE.COM/people/a#fragment"
		}},
		{"unsafe public URL", validNamespace, func(r *personenrichment.Result) {
			r.CanonicalPublicURLs[0] = "https://user:secret@example.com/people/a"
		}},
		{"empty namespace", "", func(*personenrichment.Result) {}},
		{"malformed namespace", "exa:https://provider.invalid", func(*personenrichment.Result) {}},
		{"uppercase namespace digest", "exa:" + strings.Repeat("A", 64), func(*personenrichment.Result) {}},
		{"non-ASCII provider kind", "éxa:" + strings.Repeat("a", 64), func(*personenrichment.Result) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := claimCommitResult()
			test.mutate(&result)
			_, commitErr := personenrichment.NewClaimCommit(
				validClaimCommitInput(test.namespace), result, hasher,
			)
			require.Error(t, commitErr)
		})
	}
}

func TestNewClaimCommitRejectsZeroValueSuppressionHasher(t *testing.T) {
	commit, err := personenrichment.NewClaimCommit(
		validClaimCommitInput("exa:"+strings.Repeat("8", 64)),
		claimCommitResult(),
		&personenrichment.SuppressionHasher{},
	)
	require.Error(t, err)
	_, manifestErr := commit.VerifiedReturnedIdentifierDigests()
	require.Error(t, manifestErr)
}

func TestClaimCommitRejectsZeroUnsealedOrPubliclyMutatedValues(t *testing.T) {
	requirements := require.New(t)
	var zero personenrichment.ClaimCommit
	_, err := zero.VerifiedReturnedIdentifierDigests()
	requirements.Error(err)

	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x81}, 32))
	requirements.NoError(err)
	commit, err := personenrichment.NewClaimCommit(
		validClaimCommitInput("exa:"+strings.Repeat("1", 64)), claimCommitResult(), hasher,
	)
	requirements.NoError(err)
	commit.ProviderNamespace = "exa:" + strings.Repeat("2", 64)
	_, err = commit.VerifiedReturnedIdentifierDigests()
	requirements.Error(err)

	unsealed := personenrichment.ClaimCommit{
		AttemptID: 1, RunID: 2, LeaseFence: 3, PersonID: 4,
		ProfileFingerprint: strings.Repeat("3", 64),
		ProviderNamespace:  "exa:" + strings.Repeat("4", 64),
		RequestHash:        strings.Repeat("5", 64),
	}
	_, err = unsealed.VerifiedReturnedIdentifierDigests()
	requirements.Error(err)
}

func validClaimCommitInput(namespace string) personenrichment.ClaimCommitInput {
	return personenrichment.ClaimCommitInput{
		AttemptID: 11, RunID: 12, LeaseFence: 13, PersonID: 14,
		ProfileFingerprint: strings.Repeat("6", 64),
		ProviderNamespace:  namespace,
		RequestHash:        strings.Repeat("7", 64),
		IdentityAssessment: personenrichment.IdentityAssessment{
			Accepted: true, Score: 1000, Reason: "strong_identifier_match",
			MatchedClasses: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		},
	}
}

func claimCommitResult() personenrichment.Result {
	subjectID := int64(7)
	return personenrichment.Result{
		State: personenrichment.ResultComplete, ProviderVersion: "provider-v1",
		Claims: []personfacts.ProposedClaim{{
			SubmittedValue: json.RawMessage(`"original"`),
			Evidence: []personfacts.EvidenceInput{{
				SubjectPersonID: &subjectID,
				Excerpt:         "original excerpt",
			}},
		}},
		Citations: []personenrichment.Citation{{Title: "original title"}},
		ProviderPersonIDs: []personenrichment.ProviderPersonID{
			{ID: "CaseSensitive-ID", Confidence: 1000},
			{ID: "caseSensitive-ID", Confidence: 900},
		},
		CanonicalPublicURLs: []string{
			"https://example.com/people/a",
			"https://example.com/people/b",
		},
		IdentityMatches: []personenrichment.IdentityMatch{{
			Class: personenrichment.IdentifierEmail, Value: "person@example.com", Confidence: 1000,
		}},
	}
}

func expectedSuppressionDigest(
	key []byte,
	providerNamespace string,
	class personenrichment.SuppressionIdentifierClass,
	version string,
	normalized string,
) personenrichment.SuppressionDigest {
	keyID := sha256.Sum256([]byte(suppressionTestDomain + "\x00key-id\x00" + string(key)))
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write([]byte(
		suppressionTestDomain + "\x00" + providerNamespace + "\x00" + string(class) + "\x00" + version + "\x00" + normalized,
	))
	return personenrichment.SuppressionDigest{
		ProviderNamespace: providerNamespace, IdentifierClass: class,
		NormalizationVersion: version, KeyID: hex.EncodeToString(keyID[:]), Digest: digest.Sum(nil),
	}
}

func compareSuppressionDigests(a, b personenrichment.SuppressionDigest) int {
	if value := strings.Compare(string(a.IdentifierClass), string(b.IdentifierClass)); value != 0 {
		return value
	}
	if value := strings.Compare(a.NormalizationVersion, b.NormalizationVersion); value != 0 {
		return value
	}
	return bytes.Compare(a.Digest, b.Digest)
}
