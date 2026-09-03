package personenrichment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

const (
	suppressionDomainV1                   = "msgvault/person-enrichment/suppression/v1"
	returnedIdentifierVerificationVersion = "returned-identifiers-v1"
	minimumSuppressionKeyBytes            = 32
)

type SuppressionHasher struct {
	key   []byte
	keyID string
}

func NewSuppressionHasher(key []byte) (*SuppressionHasher, error) {
	if len(key) < minimumSuppressionKeyBytes {
		return nil, fmt.Errorf("suppression key must contain at least %d bytes", minimumSuppressionKeyBytes)
	}
	ownedKey := slices.Clone(key)
	return &SuppressionHasher{key: ownedKey, keyID: suppressionKeyID(ownedKey)}, nil
}

func suppressionKeyID(key []byte) string {
	keyIDInput := make([]byte, 0, len(suppressionDomainV1)+len(key)+9)
	keyIDInput = append(keyIDInput, suppressionDomainV1...)
	keyIDInput = append(keyIDInput, "\x00key-id\x00"...)
	keyIDInput = append(keyIDInput, key...)
	digest := sha256.Sum256(keyIDInput)
	clear(keyIDInput)
	return hex.EncodeToString(digest[:])
}

func (h *SuppressionHasher) validate() error {
	if h == nil {
		return errors.New("suppression hasher is required")
	}
	if len(h.key) < minimumSuppressionKeyBytes || !isSHA256Hex(h.keyID) {
		return errors.New("suppression hasher is invalid")
	}
	wantKeyID := suppressionKeyID(h.key)
	if !hmac.Equal([]byte(wantKeyID), []byte(h.keyID)) {
		return errors.New("suppression hasher is invalid")
	}
	return nil
}

// KeyID returns the non-secret identifier for the configured suppression key.
func (h *SuppressionHasher) KeyID() (string, error) {
	if err := h.validate(); err != nil {
		return "", err
	}
	return h.keyID, nil
}

func (h *SuppressionHasher) Digest(
	providerNamespace string,
	class SuppressionIdentifierClass,
	version string,
	normalized string,
) SuppressionDigest {
	if err := h.validate(); err != nil {
		return SuppressionDigest{}
	}
	mac := hmac.New(sha256.New, h.key)
	_, _ = mac.Write([]byte(suppressionDomainV1))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(providerNamespace))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(class))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(version))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(normalized))
	return SuppressionDigest{
		ProviderNamespace: providerNamespace, IdentifierClass: class,
		NormalizationVersion: version, KeyID: h.keyID, Digest: mac.Sum(nil),
	}
}

func NewClaimCommit(input ClaimCommitInput, result Result, hasher *SuppressionHasher) (ClaimCommit, error) {
	if err := hasher.validate(); err != nil {
		return ClaimCommit{}, err
	}
	if err := validateClaimCommitInput(input); err != nil {
		return ClaimCommit{}, err
	}
	if err := result.Validate(); err != nil {
		return ClaimCommit{}, fmt.Errorf("validate enrichment result: %w", err)
	}
	if result.State != ResultComplete {
		return ClaimCommit{}, errors.New("claim commit requires a complete result")
	}

	resultCopy := cloneResult(result)
	// IdentityMatch.Value is transient adapter output. The host assesses it
	// before constructing a commit; the sink must never receive or persist it.
	resultCopy.IdentityMatches = nil
	digests, err := verifyReturnedIdentifiers(&resultCopy, input.ProviderNamespace, hasher)
	if err != nil {
		return ClaimCommit{}, err
	}
	manifest := verifiedReturnedIdentifierManifest{
		verificationVersion: returnedIdentifierVerificationVersion,
		providerNamespace:   input.ProviderNamespace,
		digests:             cloneSuppressionDigests(digests),
	}
	manifest.coverageHash, err = returnedIdentifierCoverageHash(manifest)
	if err != nil {
		return ClaimCommit{}, err
	}

	assessment := input.IdentityAssessment
	assessment.MatchedClasses = slices.Clone(input.IdentityAssessment.MatchedClasses)
	return ClaimCommit{
		AttemptID: input.AttemptID, RunID: input.RunID, LeaseFence: input.LeaseFence,
		PersonID: input.PersonID, ProfileFingerprint: input.ProfileFingerprint,
		ProviderNamespace: input.ProviderNamespace, RequestHash: input.RequestHash,
		IdentityAssessment: assessment, result: resultCopy, returnedIdentifiers: manifest,
	}, nil
}

func (c ClaimCommit) Result() Result {
	return cloneResult(c.result)
}

func (c ClaimCommit) VerifiedReturnedIdentifierDigests() ([]SuppressionDigest, error) {
	manifest := c.returnedIdentifiers
	if manifest.verificationVersion != returnedIdentifierVerificationVersion ||
		manifest.providerNamespace == "" || manifest.providerNamespace != c.ProviderNamespace ||
		!isSHA256Hex(manifest.coverageHash) {
		return nil, errors.New("claim commit has no verified returned-identifier manifest")
	}
	for _, digest := range manifest.digests {
		if digest.ProviderNamespace != manifest.providerNamespace ||
			digest.KeyID == "" || len(digest.Digest) != sha256.Size ||
			!validSuppressionIdentifierClass(digest.IdentifierClass) ||
			strings.TrimSpace(digest.NormalizationVersion) == "" {
			return nil, errors.New("claim commit returned-identifier manifest is invalid")
		}
	}
	want, err := returnedIdentifierCoverageHash(manifest)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(want), []byte(manifest.coverageHash)) {
		return nil, errors.New("claim commit returned-identifier manifest coverage changed")
	}
	return cloneSuppressionDigests(manifest.digests), nil
}

func validateClaimCommitInput(input ClaimCommitInput) error {
	if input.AttemptID <= 0 || input.RunID <= 0 || input.PersonID <= 0 || input.LeaseFence <= 0 {
		return errors.New("claim commit requires positive attempt, run, person, and lease-fence IDs")
	}
	if !isSHA256Hex(input.ProfileFingerprint) {
		return errors.New("claim commit profile fingerprint must be lowercase SHA-256")
	}
	if !validProviderNamespace(input.ProviderNamespace) {
		return errors.New("claim commit provider namespace is invalid")
	}
	if !isSHA256Hex(input.RequestHash) {
		return errors.New("claim commit request hash must be lowercase SHA-256")
	}
	if err := input.IdentityAssessment.Validate(); err != nil {
		return fmt.Errorf("validate claim commit identity assessment: %w", err)
	}
	return nil
}

func validProviderNamespace(namespace string) bool {
	kind, digest, found := strings.Cut(namespace, ":")
	if !found || kind == "" || !isSHA256Hex(digest) {
		return false
	}
	for i := range len(kind) {
		value := kind[i]
		if (i == 0 && (value < 'a' || value > 'z')) ||
			(i > 0 && (value < 'a' || value > 'z') && (value < '0' || value > '9') &&
				value != '_' && value != '-') {
			return false
		}
	}
	return true
}

func verifyReturnedIdentifiers(
	result *Result,
	providerNamespace string,
	hasher *SuppressionHasher,
) ([]SuppressionDigest, error) {
	type normalizedTuple struct {
		class      SuppressionIdentifierClass
		version    string
		normalized string
	}
	tuples := make([]normalizedTuple, 0, len(result.ProviderPersonIDs)+len(result.CanonicalPublicURLs))
	seen := make(map[string]struct{}, cap(tuples))
	appendTuple := func(class SuppressionIdentifierClass, version, normalized string) error {
		key := string(class) + "\x00" + normalized
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("returned %s identifiers must be unique after normalization", class)
		}
		seen[key] = struct{}{}
		tuples = append(tuples, normalizedTuple{class: class, version: version, normalized: normalized})
		return nil
	}

	for i := range result.ProviderPersonIDs {
		normalized, err := NormalizeSuppressionIdentifier(
			SuppressionProviderPersonID, []string{result.ProviderPersonIDs[i].ID},
		)
		if err != nil {
			return nil, fmt.Errorf("returned provider person ID %d is invalid", i)
		}
		result.ProviderPersonIDs[i].ID = normalized.Value
		if err := appendTuple(normalized.Class, normalized.NormalizationVersion, normalized.Value); err != nil {
			return nil, err
		}
	}
	for i, rawURL := range result.CanonicalPublicURLs {
		normalized, err := NormalizeSuppressionIdentifier(SuppressionPublicProfileURL, []string{rawURL})
		if err != nil || normalized.Value != rawURL {
			return nil, fmt.Errorf("returned public profile URL %d is not canonical and safe", i)
		}
		if err := appendTuple(normalized.Class, normalized.NormalizationVersion, normalized.Value); err != nil {
			return nil, err
		}
	}

	digests := make([]SuppressionDigest, len(tuples))
	for i, tuple := range tuples {
		digests[i] = hasher.Digest(providerNamespace, tuple.class, tuple.version, tuple.normalized)
		tuples[i].normalized = ""
	}
	sortSuppressionDigests(digests)
	return digests, nil
}

func returnedIdentifierCoverageHash(manifest verifiedReturnedIdentifierManifest) (string, error) {
	type coverageDigest struct {
		ProviderNamespace    string                     `json:"provider_namespace"`
		IdentifierClass      SuppressionIdentifierClass `json:"identifier_class"`
		NormalizationVersion string                     `json:"normalization_version"`
		KeyID                string                     `json:"key_id"`
		Digest               []byte                     `json:"digest"`
	}
	covered := make([]coverageDigest, len(manifest.digests))
	for i, digest := range manifest.digests {
		covered[i] = coverageDigest{
			ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
			NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID,
			Digest: slices.Clone(digest.Digest),
		}
	}
	encoded, err := json.Marshal(struct {
		VerificationVersion string           `json:"verification_version"`
		ProviderNamespace   string           `json:"provider_namespace"`
		Digests             []coverageDigest `json:"digests"`
	}{manifest.verificationVersion, manifest.providerNamespace, covered})
	if err != nil {
		return "", fmt.Errorf("encode returned-identifier coverage: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func cloneSuppressionDigests(digests []SuppressionDigest) []SuppressionDigest {
	cloned := make([]SuppressionDigest, len(digests))
	for i, digest := range digests {
		cloned[i] = digest
		cloned[i].Digest = slices.Clone(digest.Digest)
	}
	return cloned
}

func sortSuppressionDigests(digests []SuppressionDigest) {
	sort.Slice(digests, func(i, j int) bool {
		if digests[i].IdentifierClass != digests[j].IdentifierClass {
			return digests[i].IdentifierClass < digests[j].IdentifierClass
		}
		if digests[i].NormalizationVersion != digests[j].NormalizationVersion {
			return digests[i].NormalizationVersion < digests[j].NormalizationVersion
		}
		return bytes.Compare(digests[i].Digest, digests[j].Digest) < 0
	})
}

func validSuppressionIdentifierClass(class SuppressionIdentifierClass) bool {
	switch class {
	case SuppressionEmail, SuppressionPhone, SuppressionPublicProfileURL,
		SuppressionProviderPersonID, SuppressionNameCompany:
		return true
	default:
		return false
	}
}

func cloneResult(result Result) Result {
	cloned := result
	cloned.Claims = make([]personfacts.ProposedClaim, len(result.Claims))
	for i, claim := range result.Claims {
		cloned.Claims[i] = claim
		cloned.Claims[i].Target = claim.Target
		cloned.Claims[i].Target.Choices = slices.Clone(claim.Target.Choices)
		cloned.Claims[i].Target.Fields = slices.Clone(claim.Target.Fields)
		cloned.Claims[i].SubmittedValue = slices.Clone(claim.SubmittedValue)
		cloned.Claims[i].Evidence = make([]personfacts.EvidenceInput, len(claim.Evidence))
		for j, evidence := range claim.Evidence {
			cloned.Claims[i].Evidence[j] = cloneEvidenceInput(evidence)
		}
		cloned.Claims[i].ValidFrom = cloneTimePointer(claim.ValidFrom)
		cloned.Claims[i].ValidUntil = cloneTimePointer(claim.ValidUntil)
	}
	cloned.Citations = slices.Clone(result.Citations)
	cloned.ProviderPersonIDs = slices.Clone(result.ProviderPersonIDs)
	cloned.CanonicalPublicURLs = slices.Clone(result.CanonicalPublicURLs)
	cloned.IdentityMatches = slices.Clone(result.IdentityMatches)
	cloned.SourceAttempts = slices.Clone(result.SourceAttempts)
	return cloned
}

func cloneEvidenceInput(input personfacts.EvidenceInput) personfacts.EvidenceInput {
	cloned := input
	cloned.SubjectPersonID = cloneInt64Pointer(input.SubjectPersonID)
	cloned.SpanStart = cloneInt64Pointer(input.SpanStart)
	cloned.SpanEnd = cloneInt64Pointer(input.SpanEnd)
	return cloned
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type CredentialLookup func(string) (string, bool)

// ProviderCredentialLookup resolves a credential from the complete immutable
// provider profile. Stable names and canonical endpoints remain available to
// credential stores that bind secrets more narrowly than an environment name.
type ProviderCredentialLookup func(ProviderProfile) (string, bool, error)

type EgressInput struct {
	Request                Request
	Profile                ProviderProfile
	KnownProviderPersonIDs []string
}

type Authorization struct {
	Credential           string
	DisclosedIdentifiers []SuppressionDigest
	CheckedIdentifiers   []SuppressionDigest
}

type EgressGate struct {
	Consent                  ConsentChecker
	Suppressions             SuppressionChecker
	Hasher                   *SuppressionHasher
	LookupCredential         CredentialLookup
	LookupProviderCredential ProviderCredentialLookup
}

// NewProviderBoundEgressGate constructs a gate whose late credential lookup
// receives the consented provider profile. Resolution still happens only
// after consent and every suppression check succeeds.
func NewProviderBoundEgressGate(
	consent ConsentChecker,
	suppressions SuppressionChecker,
	hasher *SuppressionHasher,
	lookupProviderCredential ProviderCredentialLookup,
) (*EgressGate, error) {
	gate := &EgressGate{
		Consent: consent, Suppressions: suppressions,
		Hasher: hasher, LookupProviderCredential: lookupProviderCredential,
	}
	if err := gate.validate(); err != nil {
		return nil, err
	}
	return gate, nil
}

func NewEgressGate(
	consent ConsentChecker,
	suppressions SuppressionChecker,
	hasher *SuppressionHasher,
	lookupCredential CredentialLookup,
) (*EgressGate, error) {
	gate := &EgressGate{
		Consent: consent, Suppressions: suppressions,
		Hasher: hasher, LookupCredential: lookupCredential,
	}
	if err := gate.validate(); err != nil {
		return nil, err
	}
	return gate, nil
}

func (g EgressGate) validate() error {
	if g.Consent == nil {
		return errors.New("consent checker is required")
	}
	if g.Suppressions == nil {
		return errors.New("suppression checker is required")
	}
	if g.Hasher == nil {
		return errors.New("suppression hasher is required")
	}
	if err := g.Hasher.validate(); err != nil {
		return err
	}
	if g.LookupCredential == nil && g.LookupProviderCredential == nil {
		return errors.New("credential lookup is required")
	}
	return nil
}

func (g EgressGate) Authorize(ctx context.Context, input EgressInput) (Authorization, error) {
	if err := g.validate(); err != nil {
		return Authorization{}, err
	}
	if err := input.Profile.Validate(); err != nil {
		return Authorization{}, fmt.Errorf("validate person enrichment profile: %w", err)
	}
	active, err := g.Consent.HasActivePersonEnrichmentConsent(ctx, input.Profile.Fingerprint)
	if err != nil {
		return Authorization{}, fmt.Errorf("check person enrichment consent: %w", err)
	}
	if !active {
		return Authorization{}, ErrConsentRequired
	}
	keyIDs, err := g.Suppressions.ListPersonEnrichmentSuppressionKeyIDsContext(ctx)
	if err != nil {
		return Authorization{}, fmt.Errorf("load person enrichment suppression key IDs: %w", err)
	}
	for _, keyID := range keyIDs {
		if keyID != g.Hasher.keyID {
			return Authorization{}, ErrSuppressionKeyMismatch
		}
	}

	disclosed, checked, err := g.suppressionDigests(input)
	if err != nil {
		return Authorization{}, err
	}
	for _, digest := range checked {
		suppressed, lookupErr := g.Suppressions.HasPersonEnrichmentSuppressionContext(ctx, digest)
		if lookupErr != nil {
			return Authorization{}, fmt.Errorf("check person enrichment suppression: %w", lookupErr)
		}
		if suppressed {
			return Authorization{}, ErrSuppressed
		}
	}

	var credential string
	var ok bool
	if g.LookupProviderCredential != nil {
		credential, ok, err = g.LookupProviderCredential(input.Profile)
		if err != nil {
			return Authorization{}, fmt.Errorf("resolve person enrichment provider credential: %w", err)
		}
	} else {
		credential, ok = g.LookupCredential(input.Profile.APIKeyEnv)
	}
	if !ok || credential == "" {
		return Authorization{}, ErrCredentialUnavailable
	}
	return Authorization{
		Credential: credential, DisclosedIdentifiers: cloneSuppressionDigests(disclosed),
		CheckedIdentifiers: cloneSuppressionDigests(checked),
	}, nil
}

func (g EgressGate) suppressionDigests(input EgressInput) ([]SuppressionDigest, []SuppressionDigest, error) {
	allowed := make(map[IdentifierClass]struct{}, len(input.Profile.AllowedIdentifiers))
	for _, class := range input.Profile.AllowedIdentifiers {
		allowed[class] = struct{}{}
	}
	type rawIdentifier struct {
		class     SuppressionIdentifierClass
		values    []string
		disclosed bool
	}
	raw := make([]rawIdentifier, 0, 4+len(input.Request.Identity.PublicProfileURLs)+len(input.KnownProviderPersonIDs))
	appendScalar := func(identifierClass IdentifierClass, class SuppressionIdentifierClass, value string) error {
		if value == "" {
			return nil
		}
		if _, ok := allowed[identifierClass]; !ok {
			return errors.New("request discloses an identifier outside the provider profile")
		}
		raw = append(raw, rawIdentifier{class: class, values: []string{value}, disclosed: true})
		return nil
	}
	if err := appendScalar(IdentifierEmail, SuppressionEmail, input.Request.Identity.Email); err != nil {
		return nil, nil, err
	}
	if err := appendScalar(IdentifierPhone, SuppressionPhone, input.Request.Identity.Phone); err != nil {
		return nil, nil, err
	}
	for _, profileURL := range input.Request.Identity.PublicProfileURLs {
		if err := appendScalar(IdentifierPublicProfileURL, SuppressionPublicProfileURL, profileURL); err != nil {
			return nil, nil, err
		}
	}
	name := input.Request.Identity.Name
	company := input.Request.Identity.CurrentCompany
	if (name == "") != (company == "") {
		return nil, nil, errors.New("name and current company must be disclosed together for suppression")
	}
	if name != "" {
		if _, ok := allowed[IdentifierName]; !ok {
			return nil, nil, errors.New("request discloses an identifier outside the provider profile")
		}
	}
	if company != "" {
		if _, ok := allowed[IdentifierCurrentCompany]; !ok {
			return nil, nil, errors.New("request discloses an identifier outside the provider profile")
		}
	}
	if name != "" && company != "" {
		raw = append(raw, rawIdentifier{
			class: SuppressionNameCompany, values: []string{name, company}, disclosed: true,
		})
	}
	for _, providerPersonID := range input.KnownProviderPersonIDs {
		raw = append(raw, rawIdentifier{
			class: SuppressionProviderPersonID, values: []string{providerPersonID},
		})
	}

	disclosed := make([]SuppressionDigest, 0, len(raw))
	checked := make([]SuppressionDigest, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, identifier := range raw {
		normalized, err := NormalizeSuppressionIdentifier(identifier.class, identifier.values)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid disclosed %s identifier", identifier.class)
		}
		key := string(normalized.Class) + "\x00" + normalized.Value
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		digest := g.Hasher.Digest(
			input.Profile.ProviderNamespace,
			normalized.Class,
			normalized.NormalizationVersion,
			normalized.Value,
		)
		checked = append(checked, digest)
		if identifier.disclosed {
			disclosed = append(disclosed, digest)
		}
	}
	sortSuppressionDigests(checked)
	sortSuppressionDigests(disclosed)
	return disclosed, checked, nil
}
