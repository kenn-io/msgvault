package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

var _ personenrichment.ClaimSink = (*Store)(nil)

var errPersonEnrichmentResultEnvelopeChanged = errors.New("person enrichment result envelope changed")

const (
	maxEnrichmentCitationKeyRunes       = 256
	maxEnrichmentCitationTitleRunes     = 500
	maxEnrichmentCitationPublisherRunes = 500
	providerIdentityLockDomainV1        = "msgvault/person-enrichment/provider-identity/v1"
)

// PersonEnrichmentCitation is the bounded public citation metadata linked to
// one provider attempt. It contains no credential or private identifier.
type PersonEnrichmentCitation struct {
	ID           int64      `json:"id"`
	PersonID     int64      `json:"person_id"`
	CitationKey  string     `json:"citation_key"`
	CanonicalURL string     `json:"canonical_url"`
	Title        string     `json:"title"`
	Publisher    string     `json:"publisher"`
	Excerpt      string     `json:"excerpt"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	RetrievedAt  time.Time  `json:"retrieved_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

type preparedEnrichmentCommit struct {
	Commit                      personenrichment.ClaimCommit
	Profile                     personenrichment.ProviderProfile
	Generation                  personfacts.PreparedGeneration
	OwnershipRejectedGeneration personfacts.PreparedGeneration
	CompletionTime              time.Time
	Citations                   []personenrichment.Citation
	Digests                     []personenrichment.SuppressionDigest
}

type enrichmentCommitDisposition struct {
	Status            personenrichment.ClaimOutcomeStatus
	OwnershipConflict bool
	Replay            bool
	GenerationKey     string
}

type externalEvidenceAligner struct {
	AttemptID         int64
	ProviderRequestID string
	ProviderJobID     string
	CitationKeys      map[string]struct{}
	CitationURLs      map[string]string
}

func (a externalEvidenceAligner) Align(
	_ context.Context, input personfacts.EvidenceInput,
) (personfacts.AlignmentResult, error) {
	reject := func(detail string) (personfacts.AlignmentResult, error) {
		return personfacts.AlignmentResult{Failure: &personfacts.ValidationFailure{
			Action: personfacts.DecisionInvalid, Reason: personfacts.ReasonUnalignedEvidence,
			Detail: detail,
		}}, nil
	}
	switch input.SourceClass {
	case personfacts.EvidencePublic:
		if strings.TrimSpace(input.SourceRef) == "" {
			return reject("public enrichment evidence has no citation key")
		}
		if _, ok := a.CitationKeys[input.SourceRef]; !ok {
			return reject("public enrichment evidence citation is not in the validated result")
		}
		if !isCanonicalEnrichmentHTTPSURL(input.SourceURL) {
			return reject("public enrichment evidence URL is not canonical HTTPS")
		}
		if expectedURL, ok := a.CitationURLs[input.SourceRef]; ok && input.SourceURL != expectedURL {
			return reject("public enrichment evidence URL does not match its citation")
		}
	case personfacts.EvidenceProviderAssertion:
		if input.SourceRef != a.providerAssertionRef() {
			return reject("provider assertion does not identify this enrichment attempt")
		}
		if input.SourceURL != "" && !isCanonicalEnrichmentHTTPSURL(input.SourceURL) {
			return reject("provider assertion URL is not canonical HTTPS")
		}
	default:
		return reject("enrichment evidence uses an unsupported source class")
	}
	version := input.SourceVersion
	if version == "" {
		version = enrichmentEvidenceVersion(input)
	}
	contentHash := input.ContentSHA256
	if contentHash == "" {
		contentHash = enrichmentEvidenceContentHash(input)
	}
	return personfacts.AlignmentResult{
		Accepted: true, SourceVersion: version, ContentSHA256: contentHash,
	}, nil
}

func (a externalEvidenceAligner) providerAssertionRef() string {
	opaqueID := a.ProviderJobID
	if opaqueID == "" {
		opaqueID = a.ProviderRequestID
	}
	if a.AttemptID <= 0 || opaqueID == "" {
		return ""
	}
	return "enrichment-attempt:" + strconv.FormatInt(a.AttemptID, 10) + ":job:" + opaqueID
}

func (s *Store) CommitEnrichmentClaims(
	ctx context.Context, commit personenrichment.ClaimCommit,
) (*personenrichment.ClaimOutcome, error) {
	prepared, err := s.preparePersonEnrichmentCommit(ctx, commit)
	if err != nil {
		return nil, err
	}
	return s.commitPreparedPersonEnrichmentResult(ctx, prepared)
}

func (s *Store) preparePersonEnrichmentCommit(
	ctx context.Context, commit personenrichment.ClaimCommit,
) (*preparedEnrichmentCommit, error) {
	digests, err := commit.VerifiedReturnedIdentifierDigests()
	if err != nil {
		return nil, err
	}
	result := commit.Result()
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("validate enrichment result: %w", err)
	}
	if result.State != personenrichment.ResultComplete {
		return nil, errors.New("person enrichment claim sink requires a complete result")
	}
	if err := validateEnrichmentHostIdentityAssessment(commit.IdentityAssessment, result); err != nil {
		return nil, err
	}
	attempt, err := s.loadPersonEnrichmentCommitAttempt(ctx, s.db, commit.AttemptID, false)
	if err != nil {
		return nil, err
	}
	if attempt.LeaseFence != commit.LeaseFence {
		return nil, ErrStaleLease
	}
	if attempt.State == "starting" {
		if err := verifyPersonEnrichmentCommitAttemptIdentity(commit, attempt); err != nil {
			return nil, err
		}
		if attempt.ProviderRequestID.Valid || attempt.ProviderJobID.Valid ||
			attempt.AdapterVersion.Valid || attempt.SchemaVersion.Valid || attempt.GeneratedSchema ||
			attempt.GeneratedSchemaHash.Valid || attempt.ProgramFingerprint.Valid {
			return nil, fmt.Errorf(
				"%w: partial synchronous provider metadata", errPersonEnrichmentResultEnvelopeChanged)
		}
	} else if err := verifyPersonEnrichmentCommitAttemptEnvelope(commit, result, attempt); err != nil {
		return nil, err
	}
	profile, err := s.loadPersonEnrichmentProfile(ctx, s.db, commit.ProfileFingerprint, false)
	if err != nil {
		return nil, err
	}
	if profile.ProviderNamespace != commit.ProviderNamespace {
		return nil, fmt.Errorf("%w: provider namespace", errPersonEnrichmentResultEnvelopeChanged)
	}
	completionTime := s.personEnrichmentTime()
	if attempt.CompletedAt.Valid {
		completionTime = attempt.CompletedAt.Time
	}
	citations, citationKeys, err := validateEnrichmentCitations(result.Citations)
	if err != nil {
		return nil, err
	}
	if err := validateEnrichmentSourceAttempts(result.SourceAttempts); err != nil {
		return nil, err
	}
	claims, err := translateEnrichmentClaims(
		result.Claims, citations, commit.PersonID, commit.AttemptID, result, completionTime)
	if err != nil {
		return nil, err
	}
	claims, err = claimsWithIdentityScore(claims, commit.IdentityAssessment.Score)
	if err != nil {
		return nil, err
	}
	programFingerprint, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
		HostMappingVersion: personenrichment.HostClaimMappingVersion,
		AdapterVersion:     result.AdapterVersion, WireSchemaVersion: result.SchemaVersion,
		GeneratedSchema: result.GeneratedSchema, GeneratedSchemaHash: result.GeneratedSchemaHash,
	})
	if err != nil {
		return nil, err
	}
	if attempt.State != "starting" && attempt.ProgramFingerprint.String != programFingerprint {
		return nil, personenrichment.ErrProgramFingerprintMismatch
	}
	input := personfacts.GenerationInput{
		PersonID: commit.PersonID,
		SourceCursors: []personfacts.SourceCursor{{
			Lane:  "enrichment:" + profile.ProviderNamespace,
			Start: "trigger:" + attempt.TriggerKind + ":" + attempt.TriggerGeneration,
			End:   "request:" + attempt.RequestHash + ":attempt:" + strconv.FormatInt(attempt.ID, 10),
		}},
		ProgramID:          "personenrichment/" + profile.Kind,
		ProgramVersion:     result.AdapterVersion + "+schema:" + result.SchemaVersion,
		ProgramFingerprint: programFingerprint, CatalogFingerprint: profile.CatalogFingerprint,
		Provider: profile.Kind, ProviderVersion: result.ProviderVersion,
		Model: result.Model, ModelVersion: result.ModelVersion, ResolvedAt: completionTime,
		Policy: personfacts.PolicyContext{
			AllowSensitive:            profile.AllowSensitiveTargets,
			ProviderPolicyFingerprint: profile.Fingerprint,
		},
		Claims: claims,
	}
	aligner := externalEvidenceAligner{
		AttemptID: commit.AttemptID, ProviderRequestID: result.RequestID,
		ProviderJobID: result.JobID, CitationKeys: citationKeys,
		CitationURLs: citationURLs(citations),
	}
	prepared, err := personfacts.PreparePersonFactGeneration(ctx, input, aligner)
	if err != nil {
		return nil, err
	}
	ownershipClaims, err := claimsWithIdentityScore(claims, 0)
	if err != nil {
		return nil, err
	}
	ownershipInput := input
	ownershipInput.Claims = ownershipClaims
	ownershipPrepared, err := personfacts.PreparePersonFactGeneration(ctx, ownershipInput, aligner)
	if err != nil {
		return nil, err
	}
	return &preparedEnrichmentCommit{
		Commit: commit, Profile: profile, Generation: prepared,
		OwnershipRejectedGeneration: ownershipPrepared,
		CompletionTime:              completionTime, Citations: citations,
		Digests: cloneEnrichmentDigests(digests),
	}, nil
}

func (s *Store) commitPreparedPersonEnrichmentResult(
	ctx context.Context, prepared *preparedEnrichmentCommit,
) (*personenrichment.ClaimOutcome, error) {
	if prepared == nil {
		return nil, errors.New("prepared person enrichment commit is required")
	}
	outcome := &personenrichment.ClaimOutcome{}
	var costViolation bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("result_before_authority_lock")
		}
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		disposition, err := s.recheckPersonEnrichmentCommitTx(
			ctx, tx, prepared.Commit, prepared.Profile, prepared.Generation,
			prepared.OwnershipRejectedGeneration)
		if err != nil {
			return err
		}
		if disposition.Replay {
			generation, loadErr := s.loadPersonFactGenerationResultTx(
				ctx, tx, prepared.Commit.PersonID, disposition.GenerationKey)
			if loadErr != nil {
				return loadErr
			}
			outcome.Status = disposition.Status
			outcome.Generation = generation
			return nil
		}
		switch disposition.Status {
		case personenrichment.ClaimPolicyRejected:
			if err := s.rejectPersonEnrichmentResultPolicyTx(
				ctx, tx, prepared.Commit, prepared.CompletionTime); err != nil {
				return err
			}
			outcome.Status = disposition.Status
			return nil
		case personenrichment.ClaimSuppressed:
			if err := s.rejectPersonEnrichmentResultSuppressedTx(
				ctx, tx, prepared.Commit, prepared.CompletionTime); err != nil {
				return err
			}
			outcome.Status = disposition.Status
			return nil
		case personenrichment.ClaimApplied, personenrichment.ClaimIdentityRejected:
			// Both branches apply a prepared PR1 generation below.
		default:
			return fmt.Errorf("invalid person enrichment result disposition %q", disposition.Status)
		}
		result := prepared.Commit.Result()
		costViolation, err = reconcilePersonEnrichmentCostTx(
			ctx, tx, s.dialect, prepared.Commit.AttemptID, result.Cost,
			result.Cost == (personenrichment.Cost{}), prepared.CompletionTime)
		if err != nil {
			return err
		}
		if costViolation {
			return s.terminatePersonEnrichmentCostViolationTx(
				ctx, tx, prepared.Commit, prepared.CompletionTime)
		}
		generation := prepared.Generation
		if disposition.OwnershipConflict {
			generation = prepared.OwnershipRejectedGeneration
		}
		if disposition.Status == personenrichment.ClaimApplied {
			if err := s.insertPersonEnrichmentEvidenceMetadataTx(
				ctx, tx, prepared.Commit, prepared.Citations); err != nil {
				return err
			}
		}
		generationResult, err := s.applyPreparedPersonFactGenerationTx(ctx, tx, generation)
		if err != nil {
			return err
		}
		if disposition.Status == personenrichment.ClaimApplied {
			if err := s.attachPersonEnrichmentReturnedIdentitiesTx(
				ctx, tx, prepared.Commit, prepared.Digests, prepared.CompletionTime); err != nil {
				return err
			}
		}
		costViolation, err = s.completePersonEnrichmentClaimTx(
			ctx, tx, prepared.Commit, disposition.Status, generationResult,
			prepared.Profile, prepared.CompletionTime)
		if err != nil {
			return err
		}
		outcome.Status = disposition.Status
		outcome.Generation = generationResult
		return nil
	})
	if err != nil {
		return nil, err
	}
	if costViolation {
		return nil, ErrProviderCostBoundExceeded
	}
	return outcome, nil
}

func (s *Store) recheckPersonEnrichmentCommitTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit,
	profile personenrichment.ProviderProfile, generation,
	ownershipGeneration personfacts.PreparedGeneration,
) (enrichmentCommitDisposition, error) {
	result := commit.Result()
	if s.personEnrichmentTxBarrier != nil {
		s.personEnrichmentTxBarrier("result_before_person_lock")
	}
	currentRevision, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, commit.PersonID)
	if err != nil {
		return enrichmentCommitDisposition{}, err
	}
	if s.personEnrichmentTxBarrier != nil {
		s.personEnrichmentTxBarrier("result_person_locked")
	}
	attempt, err := s.loadPersonEnrichmentCommitAttempt(ctx, tx, commit.AttemptID, true)
	if err != nil {
		return enrichmentCommitDisposition{}, err
	}
	if attempt.State == "starting" {
		programFingerprint, fingerprintErr := personenrichment.ProgramFingerprint(
			personenrichment.ProgramDescriptor{
				HostMappingVersion: personenrichment.HostClaimMappingVersion,
				AdapterVersion:     result.AdapterVersion, WireSchemaVersion: result.SchemaVersion,
				GeneratedSchema: result.GeneratedSchema, GeneratedSchemaHash: result.GeneratedSchemaHash,
			})
		if fingerprintErr != nil {
			return enrichmentCommitDisposition{}, fingerprintErr
		}
		updated, updateErr := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
			SET provider_request_id = ?, provider_job_id = ?, adapter_version = ?,
			    schema_version = ?, generated_schema = ?, generated_schema_hash = ?,
			    program_fingerprint = ?
			WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?
			  AND state = 'starting' AND dispatch_authorized_at IS NOT NULL
			  AND provider_request_id IS NULL AND provider_job_id IS NULL
			  AND adapter_version IS NULL AND schema_version IS NULL
			  AND generated_schema = FALSE AND generated_schema_hash IS NULL
			  AND program_fingerprint IS NULL`, nullableOpaqueID(result.RequestID),
			nullableOpaqueID(result.JobID), strings.TrimSpace(result.AdapterVersion),
			strings.TrimSpace(result.SchemaVersion), result.GeneratedSchema,
			nullableTrimmed(result.GeneratedSchemaHash), programFingerprint,
			attempt.ID, attempt.RunID, attempt.LeaseOwner.String, attempt.LeaseFence)
		if updateErr != nil {
			return enrichmentCommitDisposition{}, fmt.Errorf(
				"bind synchronous person enrichment result: %w", updateErr)
		}
		if err := requireOneLeaseRow(updated); err != nil {
			return enrichmentCommitDisposition{}, err
		}
		attempt, err = s.loadPersonEnrichmentCommitAttempt(ctx, tx, commit.AttemptID, true)
		if err != nil {
			return enrichmentCommitDisposition{}, err
		}
	}
	if err := verifyPersonEnrichmentCommitAttemptEnvelope(commit, result, attempt); err != nil {
		return enrichmentCommitDisposition{}, err
	}
	if attempt.FactGenerationKey.Valid {
		generationKey := attempt.FactGenerationKey.String
		normalKey := generation.GenerationKey()
		ownershipKey := ownershipGeneration.GenerationKey()
		if generationKey != normalKey && generationKey != ownershipKey {
			return enrichmentCommitDisposition{}, fmt.Errorf(
				"%w: fact generation key", errPersonEnrichmentResultEnvelopeChanged)
		}
		switch attempt.State {
		case "succeeded":
			if generationKey != normalKey {
				return enrichmentCommitDisposition{}, fmt.Errorf(
					"%w: succeeded ownership generation", errPersonEnrichmentResultEnvelopeChanged)
			}
			return enrichmentCommitDisposition{
				Status: personenrichment.ClaimApplied, Replay: true, GenerationKey: generationKey,
			}, nil
		case "identity_rejected":
			return enrichmentCommitDisposition{
				Status: personenrichment.ClaimIdentityRejected, Replay: true, GenerationKey: generationKey,
			}, nil
		default:
			return enrichmentCommitDisposition{}, fmt.Errorf(
				"%w: terminal generation state", errPersonEnrichmentResultEnvelopeChanged)
		}
	}
	token := enrichmentCommitLeaseToken(commit, attempt.LeaseOwner.String)
	if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
		return enrichmentCommitDisposition{}, err
	}
	if attempt.State != "pending" && attempt.State != "starting" {
		return enrichmentCommitDisposition{}, fmt.Errorf(
			"%w: attempt state %q", errPersonEnrichmentResultEnvelopeChanged, attempt.State)
	}
	if currentRevision != attempt.PersonRevision {
		return enrichmentCommitDisposition{}, fmt.Errorf(
			"%w: person revision", errPersonEnrichmentResultEnvelopeChanged)
	}
	currentProfile, err := s.loadPersonEnrichmentProfile(ctx, tx, commit.ProfileFingerprint, true)
	if err != nil {
		return enrichmentCommitDisposition{}, err
	}
	if !reflect.DeepEqual(currentProfile, profile) {
		return enrichmentCommitDisposition{}, fmt.Errorf(
			"%w: immutable profile", errPersonEnrichmentResultEnvelopeChanged)
	}
	catalogDisposition, err := s.recheckPersonEnrichmentCatalogTx(ctx, tx, profile)
	if err != nil {
		return enrichmentCommitDisposition{}, err
	}
	if catalogDisposition == personenrichment.ClaimPolicyRejected {
		return enrichmentCommitDisposition{Status: catalogDisposition}, nil
	}
	var tracked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM person_tracking WHERE person_id = ?)`, commit.PersonID,
	).Scan(&tracked); err != nil {
		return enrichmentCommitDisposition{}, fmt.Errorf("recheck person enrichment tracking: %w", err)
	}
	if !tracked {
		return enrichmentCommitDisposition{Status: personenrichment.ClaimPolicyRejected}, nil
	}
	var consentID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM person_enrichment_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL ORDER BY id DESC LIMIT 1`+
		s.dialect.SelectForUpdate(), profile.Fingerprint).Scan(&consentID)
	if errors.Is(err, sql.ErrNoRows) {
		return enrichmentCommitDisposition{Status: personenrichment.ClaimPolicyRejected}, nil
	}
	if err != nil {
		return enrichmentCommitDisposition{}, fmt.Errorf("lock enrichment consent: %w", err)
	}
	digests, err := commit.VerifiedReturnedIdentifierDigests()
	if err != nil {
		return enrichmentCommitDisposition{}, err
	}
	disclosedDigests, err := s.loadPersonEnrichmentAttemptIdentifiersTx(
		ctx, tx, commit.AttemptID)
	if err != nil {
		return enrichmentCommitDisposition{}, err
	}
	allDigests := append([]personenrichment.SuppressionDigest(nil), disclosedDigests...)
	allDigests = append(allDigests, digests...)
	if err := s.recheckPersonEnrichmentSuppressionsTx(ctx, tx, allDigests); err != nil {
		if errors.Is(err, personenrichment.ErrSuppressed) {
			return enrichmentCommitDisposition{Status: personenrichment.ClaimSuppressed}, nil
		}
		return enrichmentCommitDisposition{}, err
	}
	providerIDs := result.ProviderPersonIDs
	sort.Slice(providerIDs, func(i, j int) bool { return providerIDs[i].ID < providerIDs[j].ID })
	ownershipConflict := false
	for _, identity := range providerIDs {
		owner, owned, err := s.lockPersonEnrichmentProviderIdentityOwnershipTx(
			ctx, tx, profile.ProviderNamespace, identity.ID)
		if err != nil {
			return enrichmentCommitDisposition{}, err
		}
		if owned && owner != commit.PersonID {
			ownershipConflict = true
		}
	}
	if ownershipConflict {
		return enrichmentCommitDisposition{
			Status: personenrichment.ClaimIdentityRejected, OwnershipConflict: true,
		}, nil
	}
	if !commit.IdentityAssessment.Accepted {
		return enrichmentCommitDisposition{Status: personenrichment.ClaimIdentityRejected}, nil
	}
	return enrichmentCommitDisposition{Status: personenrichment.ClaimApplied}, nil
}

func claimsWithIdentityScore(
	claims []personfacts.ProposedClaim, score int,
) ([]personfacts.ProposedClaim, error) {
	if score < 0 || score > 1000 {
		return nil, fmt.Errorf("identity assessment score %d must be in [0,1000]", score)
	}
	result := make([]personfacts.ProposedClaim, len(claims))
	for i := range claims {
		if len(claims[i].Evidence) == 0 {
			return nil, fmt.Errorf("enrichment claim %d has no evidence", i)
		}
		result[i] = claims[i]
		result[i].Target.Choices = slices.Clone(claims[i].Target.Choices)
		result[i].Target.Fields = slices.Clone(claims[i].Target.Fields)
		result[i].SubmittedValue = append(json.RawMessage(nil), claims[i].SubmittedValue...)
		result[i].Evidence = slices.Clone(claims[i].Evidence)
		result[i].ValidFrom = copyEnrichmentTimePointer(claims[i].ValidFrom)
		result[i].ValidUntil = copyEnrichmentTimePointer(claims[i].ValidUntil)
		for j := range result[i].Evidence {
			result[i].Evidence[j].IdentityScore = score
			result[i].Evidence[j].SubjectPersonID = copyEnrichmentInt64Pointer(
				result[i].Evidence[j].SubjectPersonID)
			result[i].Evidence[j].SpanStart = copyEnrichmentInt64Pointer(result[i].Evidence[j].SpanStart)
			result[i].Evidence[j].SpanEnd = copyEnrichmentInt64Pointer(result[i].Evidence[j].SpanEnd)
		}
	}
	return result, nil
}

func (s *Store) insertPersonEnrichmentEvidenceMetadataTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit,
	citations []personenrichment.Citation,
) error {
	for _, citation := range citations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_citations
			(person_id, citation_key, canonical_url, title, publisher, excerpt,
			 published_at, retrieved_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (person_id, citation_key) DO NOTHING`,
			commit.PersonID, citation.Key, citation.URL, citation.Title, citation.Publisher,
			citation.Excerpt, nullableEnrichmentTime(citation.PublishedAt), citation.RetrievedAt); err != nil {
			return fmt.Errorf("insert person enrichment citation: %w", err)
		}
		stored, err := scanPersonEnrichmentCitation(tx.QueryRowContext(ctx, `SELECT
			id, person_id, citation_key, canonical_url, title, publisher, excerpt,
			published_at, retrieved_at, created_at
			FROM person_enrichment_citations WHERE person_id = ? AND citation_key = ?`,
			commit.PersonID, citation.Key))
		if err != nil {
			return fmt.Errorf("load person enrichment citation: %w", err)
		}
		if !personEnrichmentCitationMatches(stored, citation) {
			return errors.New("person enrichment citation key has different immutable metadata")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_attempt_citations
			(attempt_id, citation_id) VALUES (?, ?)
			ON CONFLICT (attempt_id, citation_id) DO NOTHING`, commit.AttemptID, stored.ID); err != nil {
			return fmt.Errorf("link person enrichment attempt citation: %w", err)
		}
	}
	for _, source := range commit.Result().SourceAttempts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_attempt_sources
			(attempt_id, canonical_url, outcome, observed_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (attempt_id, canonical_url, outcome) DO NOTHING`,
			commit.AttemptID, source.URL, source.Outcome, source.ObservedAt); err != nil {
			return fmt.Errorf("insert person enrichment source attempt: %w", err)
		}
	}
	return nil
}

func (s *Store) attachPersonEnrichmentReturnedIdentitiesTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit,
	digests []personenrichment.SuppressionDigest, verifiedAt time.Time,
) error {
	for _, digest := range digests {
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_attempt_identifiers
			(attempt_id, provider_namespace, identifier_class, normalization_version, key_id, digest)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (attempt_id, provider_namespace, identifier_class,
			             normalization_version, key_id, digest) DO NOTHING`,
			commit.AttemptID, digest.ProviderNamespace, digest.IdentifierClass,
			digest.NormalizationVersion, digest.KeyID, digest.Digest); err != nil {
			return fmt.Errorf("insert person enrichment attempt identifier: %w", err)
		}
	}
	for _, identity := range commit.Result().ProviderPersonIDs {
		if err := s.attachPersonEnrichmentProviderIdentityTx(
			ctx, tx, commit.PersonID, commit.ProviderNamespace, identity, verifiedAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) attachPersonEnrichmentProviderIdentityTx(
	ctx context.Context, tx *loggedTx, personID int64, namespace string,
	identity personenrichment.ProviderPersonID, verifiedAt time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_provider_identities
		(person_id, provider_namespace, provider_person_id, confidence, verified_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (person_id, provider_namespace, provider_person_id) DO UPDATE SET
		confidence = excluded.confidence, verified_at = excluded.verified_at`,
		personID, namespace, identity.ID, identity.Confidence, verifiedAt); err != nil {
		return fmt.Errorf("attach person enrichment provider identity: %w", err)
	}
	return nil
}

func (s *Store) completePersonEnrichmentClaimTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit,
	status personenrichment.ClaimOutcomeStatus, generation *personfacts.GenerationResult,
	profile personenrichment.ProviderProfile, completionTime time.Time,
) (bool, error) {
	if generation == nil {
		return false, errors.New("person enrichment fact generation result is required")
	}
	token := enrichmentCommitLeaseToken(commit, "")
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner FROM person_enrichment_attempts
		WHERE id = ?`, commit.AttemptID).Scan(&owner); err != nil {
		return false, fmt.Errorf("load enrichment completion owner: %w", err)
	}
	token.Owner = owner
	if status == personenrichment.ClaimApplied {
		refreshAt := completionTime.Add(profile.RefreshInterval)
		return s.completePersonEnrichmentAttemptTx(ctx, tx, token, personEnrichmentAttemptCompletion{
			State: "succeeded", ActualCost: commit.Result().Cost,
			ActualCostMissing: commit.Result().Cost == (personenrichment.Cost{}),
			FactGenerationKey: generation.GenerationKey, CompletedAt: completionTime,
			RefreshAt: &refreshAt, RefreshGeneration: "refresh:" + generation.GenerationKey,
			CostReconciled: true,
		})
	}
	if status != personenrichment.ClaimIdentityRejected {
		return false, fmt.Errorf("invalid enrichment completion status %q", status)
	}
	return false, s.completePersonEnrichmentRejectedAttemptTx(
		ctx, tx, commit, "identity_rejected", personenrichment.FailureIdentityRejected,
		generation.GenerationKey, completionTime, true)
}

func (s *Store) rejectPersonEnrichmentResultPolicyTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit, completionTime time.Time,
) error {
	return s.completePersonEnrichmentRejectedAttemptTx(
		ctx, tx, commit, "terminal", personenrichment.FailurePolicy, "", completionTime, false)
}

func (s *Store) rejectPersonEnrichmentResultSuppressedTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit, completionTime time.Time,
) error {
	return s.completePersonEnrichmentRejectedAttemptTx(
		ctx, tx, commit, "suppressed", personenrichment.FailureSuppressed, "", completionTime, false)
}

func (s *Store) completePersonEnrichmentRejectedAttemptTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit,
	state string, failure personenrichment.FailureClass, generationKey string, completionTime time.Time,
	costReconciled bool,
) error {
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner FROM person_enrichment_attempts
		WHERE id = ?`, commit.AttemptID).Scan(&owner); err != nil {
		return fmt.Errorf("load rejected enrichment completion owner: %w", err)
	}
	token := enrichmentCommitLeaseToken(commit, owner)
	if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
		return err
	}
	if !costReconciled {
		result := commit.Result()
		if _, err := reconcilePersonEnrichmentCostTx(ctx, tx, s.dialect, commit.AttemptID,
			result.Cost, result.Cost == (personenrichment.Cost{}), completionTime); err != nil {
			return err
		}
	}
	updated, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
		SET state = ?, failure_class = ?, fact_generation_key = ?, completed_at = ?,
		    next_action_at = NULL, lease_owner = NULL, lease_until = NULL
		WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
		state, failure, nullableTrimmed(generationKey), completionTime,
		commit.AttemptID, commit.RunID, owner, commit.LeaseFence)
	if err != nil {
		return fmt.Errorf("complete rejected person enrichment attempt: %w", err)
	}
	if err := requireOneLeaseRow(updated); err != nil {
		return err
	}
	return deleteTerminalEnrichmentWorkTx(ctx, tx, token)
}

func (s *Store) terminatePersonEnrichmentCostViolationTx(
	ctx context.Context, tx *loggedTx, commit personenrichment.ClaimCommit, completionTime time.Time,
) error {
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner FROM person_enrichment_attempts
		WHERE id = ?`, commit.AttemptID).Scan(&owner); err != nil {
		return fmt.Errorf("load cost-violating enrichment completion owner: %w", err)
	}
	token := enrichmentCommitLeaseToken(commit, owner)
	if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
		SET state = 'terminal', failure_class = ?, fact_generation_key = NULL,
		    completed_at = ?, next_action_at = NULL, lease_owner = NULL, lease_until = NULL
		WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
		personenrichment.FailureTerminal, completionTime, commit.AttemptID, commit.RunID,
		owner, commit.LeaseFence)
	if err != nil {
		return fmt.Errorf("complete cost-violating person enrichment attempt: %w", err)
	}
	if err := requireOneLeaseRow(updated); err != nil {
		return err
	}
	return deleteTerminalEnrichmentWorkTx(ctx, tx, token)
}

func (s *Store) lockPersonEnrichmentProviderIdentityOwnershipTx(
	ctx context.Context, tx *loggedTx, namespace, providerPersonID string,
) (int64, bool, error) {
	if s.personEnrichmentOwnershipBarrier != nil {
		s.personEnrichmentOwnershipBarrier("before_provider_identity_key", tx)
	}
	lockKey := enrichmentProviderIdentityLockKey(namespace, providerPersonID)
	if err := s.lockProfileIdentityKeyTxContext(
		ctx, tx, "person-enrichment-provider-identity", lockKey); err != nil {
		return 0, false, err
	}
	if s.personEnrichmentOwnershipBarrier != nil {
		s.personEnrichmentOwnershipBarrier("provider_identity_key_locked", tx)
	}
	var owner int64
	err := tx.QueryRowContext(ctx, `SELECT person_id FROM person_enrichment_provider_identities
		WHERE provider_namespace = ? AND provider_person_id = ?`+
		s.dialect.SelectForUpdate(), namespace, providerPersonID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lock person enrichment provider identity: %w", err)
	}
	return owner, true, nil
}

func (s *Store) ListPersonEnrichmentAttemptCitationsContext(
	ctx context.Context, attemptID int64,
) ([]PersonEnrichmentCitation, error) {
	if attemptID <= 0 {
		return nil, errors.New("person enrichment attempt ID must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.person_id, c.citation_key,
		c.canonical_url, c.title, c.publisher, c.excerpt, c.published_at,
		c.retrieved_at, c.created_at
		FROM person_enrichment_attempt_citations ac
		JOIN person_enrichment_citations c ON c.id = ac.citation_id
		WHERE ac.attempt_id = ? ORDER BY c.citation_key, c.id`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("list person enrichment attempt citations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]PersonEnrichmentCitation, 0)
	for rows.Next() {
		citation, scanErr := scanPersonEnrichmentCitation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person enrichment attempt citation: %w", scanErr)
		}
		result = append(result, citation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person enrichment attempt citations: %w", err)
	}
	return result, nil
}

func scanPersonEnrichmentCitation(row scanner) (PersonEnrichmentCitation, error) {
	var citation PersonEnrichmentCitation
	var published, retrieved, created nullableTimestamp
	if err := row.Scan(&citation.ID, &citation.PersonID, &citation.CitationKey,
		&citation.CanonicalURL, &citation.Title, &citation.Publisher, &citation.Excerpt,
		&published, &retrieved, &created); err != nil {
		return citation, err
	}
	if !retrieved.Valid || !created.Valid {
		return citation, errors.New("person enrichment citation has invalid timestamps")
	}
	citation.PublishedAt = optionalTimestamp(published)
	citation.RetrievedAt = retrieved.Time
	citation.CreatedAt = created.Time
	return citation, nil
}

type personEnrichmentCommitAttempt struct {
	ID, RunID, PersonID, PersonRevision, LeaseFence                        int64
	ProfileFingerprint, TriggerKind, TriggerGeneration, RequestHash, State string
	ProviderRequestID, ProviderJobID, AdapterVersion, SchemaVersion        sql.NullString
	GeneratedSchema                                                        bool
	GeneratedSchemaHash, ProgramFingerprint, LeaseOwner, FactGenerationKey sql.NullString
	CompletedAt                                                            nullableTimestamp
}

const personEnrichmentCommitAttemptSelect = `SELECT id, run_id, person_id,
	profile_fingerprint, trigger_kind, trigger_generation, person_revision,
	request_hash, state, provider_request_id, provider_job_id, adapter_version,
	schema_version, generated_schema, generated_schema_hash, program_fingerprint,
	lease_owner, lease_fence, fact_generation_key, completed_at
	FROM person_enrichment_attempts WHERE id = ?`

func (s *Store) loadPersonEnrichmentCommitAttempt(
	ctx context.Context, queryer contextRowQuerier, attemptID int64, lock bool,
) (personEnrichmentCommitAttempt, error) {
	var attempt personEnrichmentCommitAttempt
	query := personEnrichmentCommitAttemptSelect
	if lock {
		query += s.dialect.SelectForUpdate()
	}
	err := queryer.QueryRowContext(ctx, query, attemptID).Scan(
		&attempt.ID, &attempt.RunID, &attempt.PersonID, &attempt.ProfileFingerprint,
		&attempt.TriggerKind, &attempt.TriggerGeneration, &attempt.PersonRevision,
		&attempt.RequestHash, &attempt.State, &attempt.ProviderRequestID,
		&attempt.ProviderJobID, &attempt.AdapterVersion, &attempt.SchemaVersion,
		&attempt.GeneratedSchema, &attempt.GeneratedSchemaHash, &attempt.ProgramFingerprint,
		&attempt.LeaseOwner, &attempt.LeaseFence, &attempt.FactGenerationKey,
		&attempt.CompletedAt)
	if err != nil {
		return attempt, fmt.Errorf("load person enrichment result attempt: %w", err)
	}
	return attempt, nil
}

type storedPersonEnrichmentPolicy struct {
	Kind                            string                             `json:"kind"`
	ProviderNamespace               string                             `json:"provider_namespace"`
	CatalogFingerprint              string                             `json:"catalog_fingerprint"`
	Endpoint                        string                             `json:"endpoint"`
	PollEndpoint                    string                             `json:"poll_endpoint"`
	APIKeyEnv                       string                             `json:"api_key_env"`
	Mode                            string                             `json:"mode"`
	Tier                            string                             `json:"tier"`
	NumResults                      int                                `json:"num_results"`
	AllowedIdentifiers              []personenrichment.IdentifierClass `json:"allowed_identifiers"`
	Targets                         []personfacts.TargetDescriptor     `json:"targets"`
	AllowSensitiveTargets           bool                               `json:"allow_sensitive_targets"`
	RetentionPosture                string                             `json:"retention_posture"`
	TrainingPosture                 string                             `json:"training_posture"`
	RefreshInterval                 time.Duration                      `json:"refresh_interval"`
	MaxRequestsPerRun               int64                              `json:"max_requests_per_run"`
	MaxRequestsPerDay               int64                              `json:"max_requests_per_day"`
	MaxCostUSDMicrosPerPersonPerDay int64                              `json:"max_cost_usd_micros_per_person_per_day"`
	MaxCostUSDMicrosPerRun          int64                              `json:"max_cost_usd_micros_per_run"`
	MaxCostUSDMicrosPerDay          int64                              `json:"max_cost_usd_micros_per_day"`
}

func (s *Store) loadPersonEnrichmentProfile(
	ctx context.Context, queryer contextRowQuerier, fingerprint string, lock bool,
) (personenrichment.ProviderProfile, error) {
	var storedFingerprint, name, kind, namespace, endpoint, apiKeyEnv, policyJSON string
	query := `SELECT fingerprint, provider_name, provider_kind, provider_namespace,
		endpoint, api_key_env, CAST(policy_json AS TEXT)
		FROM person_enrichment_profiles WHERE fingerprint = ?`
	if lock {
		query += s.dialect.SelectForUpdate()
	}
	if err := queryer.QueryRowContext(ctx, query, fingerprint).Scan(
		&storedFingerprint, &name, &kind, &namespace, &endpoint, &apiKeyEnv, &policyJSON); err != nil {
		return personenrichment.ProviderProfile{}, fmt.Errorf("load person enrichment profile: %w", err)
	}
	var policy storedPersonEnrichmentPolicy
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		return personenrichment.ProviderProfile{}, fmt.Errorf("decode person enrichment profile: %w", err)
	}
	targetKeys := make([]string, len(policy.Targets))
	for i := range policy.Targets {
		targetKeys[i] = policy.Targets[i].Key
	}
	profile, err := (personenrichment.ProviderConfig{
		Name: name, Kind: policy.Kind, Enabled: true,
		Endpoint: policy.Endpoint, PollEndpoint: policy.PollEndpoint, APIKeyEnv: policy.APIKeyEnv,
		Mode: policy.Mode, Tier: policy.Tier, NumResults: policy.NumResults,
		AllowedIdentifiers: slices.Clone(policy.AllowedIdentifiers), TargetKeys: targetKeys,
		AllowSensitiveTargets: policy.AllowSensitiveTargets,
		RetentionPosture:      policy.RetentionPosture, TrainingPosture: policy.TrainingPosture,
		RefreshInterval: policy.RefreshInterval, RequestTimeout: time.Second,
		PollInterval: time.Second, MaxJobAge: time.Second,
		MaxRequestsPerRun: policy.MaxRequestsPerRun, MaxRequestsPerDay: policy.MaxRequestsPerDay,
		MaxCostUSDMicrosPerPersonPerDay: policy.MaxCostUSDMicrosPerPersonPerDay,
		MaxCostUSDMicrosPerRun:          policy.MaxCostUSDMicrosPerRun,
		MaxCostUSDMicrosPerDay:          policy.MaxCostUSDMicrosPerDay,
	}).Profile(personfacts.Catalog{Targets: policy.Targets})
	if err != nil {
		return personenrichment.ProviderProfile{}, fmt.Errorf("reconstruct person enrichment profile: %w", err)
	}
	if storedFingerprint != profile.Fingerprint || kind != profile.Kind ||
		namespace != profile.ProviderNamespace || endpoint != profile.Endpoint ||
		apiKeyEnv != profile.APIKeyEnv || !equalJSON([]byte(policyJSON), profile.PolicyJSON) {
		return personenrichment.ProviderProfile{}, fmt.Errorf(
			"%w: stored profile policy", errPersonEnrichmentResultEnvelopeChanged)
	}
	return profile, nil
}

// LoadProviderProfile returns the exact immutable policy bound to one durable
// work item. Runtime-only timeouts and credentials are deliberately absent.
func (s *Store) LoadProviderProfile(
	ctx context.Context, fingerprint string,
) (personenrichment.ProviderProfile, error) {
	profile, err := s.loadPersonEnrichmentProfile(ctx, s.db, fingerprint, false)
	if err != nil {
		return personenrichment.ProviderProfile{}, err
	}
	if err := profile.Validate(); err != nil {
		return personenrichment.ProviderProfile{}, fmt.Errorf("validate loaded person enrichment profile: %w", err)
	}
	return profile, nil
}

func verifyPersonEnrichmentCommitAttemptEnvelope(
	commit personenrichment.ClaimCommit, result personenrichment.Result,
	attempt personEnrichmentCommitAttempt,
) error {
	if err := verifyPersonEnrichmentCommitAttemptIdentity(commit, attempt); err != nil {
		return err
	}
	if nullOpaqueValue(attempt.ProviderRequestID) != result.RequestID ||
		nullOpaqueValue(attempt.ProviderJobID) != result.JobID ||
		attempt.AdapterVersion.String != result.AdapterVersion ||
		attempt.SchemaVersion.String != result.SchemaVersion ||
		attempt.GeneratedSchema != result.GeneratedSchema ||
		attempt.GeneratedSchemaHash.String != result.GeneratedSchemaHash {
		return fmt.Errorf("%w: provider result metadata", errPersonEnrichmentResultEnvelopeChanged)
	}
	return nil
}

func verifyPersonEnrichmentCommitAttemptIdentity(
	commit personenrichment.ClaimCommit, attempt personEnrichmentCommitAttempt,
) error {
	if attempt.ID != commit.AttemptID || attempt.RunID != commit.RunID ||
		attempt.PersonID != commit.PersonID || attempt.ProfileFingerprint != commit.ProfileFingerprint ||
		attempt.RequestHash != commit.RequestHash {
		return errPersonEnrichmentResultEnvelopeChanged
	}
	return nil
}

func validateEnrichmentHostIdentityAssessment(
	assessment personenrichment.IdentityAssessment, result personenrichment.Result,
) error {
	if err := assessment.Validate(); err != nil {
		return err
	}
	if !assessment.Accepted {
		if assessment.Score != 0 || assessment.Reason != "identity_not_verified" ||
			len(assessment.MatchedClasses) != 0 {
			return errors.New("failed enrichment identity assessment is not host canonical")
		}
		return nil
	}
	switch assessment.Reason {
	case "verified_provider_person_id":
		if assessment.Score != 1000 || len(assessment.MatchedClasses) != 0 {
			return errors.New("verified provider identity assessment is not host canonical")
		}
	case "strong_identifier_match":
		if assessment.Score != 1000 || len(assessment.MatchedClasses) == 0 {
			return errors.New("strong enrichment identity assessment is not host canonical")
		}
		for _, class := range assessment.MatchedClasses {
			if class != personenrichment.IdentifierEmail && class != personenrichment.IdentifierPhone &&
				class != personenrichment.IdentifierPublicProfileURL {
				return errors.New("strong enrichment identity assessment contains a weak class")
			}
		}
	case "name_company_match":
		if assessment.Score != 900 || result.IdentityConfidence < 900 ||
			!slices.Equal(assessment.MatchedClasses, []personenrichment.IdentifierClass{
				personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
			}) {
			return errors.New("name-company enrichment identity assessment is not host canonical")
		}
	default:
		return errors.New("accepted enrichment identity assessment reason is not host canonical")
	}
	return nil
}

func validateEnrichmentCitations(
	input []personenrichment.Citation,
) ([]personenrichment.Citation, map[string]struct{}, error) {
	byKey := make(map[string]personenrichment.Citation, len(input))
	for i, citation := range input {
		if citation.Key == "" || strings.TrimSpace(citation.Key) != citation.Key ||
			utf8.RuneCountInString(citation.Key) > maxEnrichmentCitationKeyRunes {
			return nil, nil, fmt.Errorf("person enrichment citation %d has invalid key", i)
		}
		if !isCanonicalEnrichmentHTTPSURL(citation.URL) {
			return nil, nil, fmt.Errorf("person enrichment citation %d URL is not canonical HTTPS", i)
		}
		if utf8.RuneCountInString(citation.Title) > maxEnrichmentCitationTitleRunes ||
			utf8.RuneCountInString(citation.Publisher) > maxEnrichmentCitationPublisherRunes ||
			utf8.RuneCountInString(citation.Excerpt) > personfacts.MaxEvidenceExcerptRunes {
			return nil, nil, fmt.Errorf("person enrichment citation %d exceeds metadata bounds", i)
		}
		if citation.RetrievedAt.IsZero() || !enrichmentTimeIsUTC(citation.RetrievedAt) ||
			(!citation.PublishedAt.IsZero() && !enrichmentTimeIsUTC(citation.PublishedAt)) {
			return nil, nil, fmt.Errorf("person enrichment citation %d has invalid timestamps", i)
		}
		citation.RetrievedAt = citation.RetrievedAt.UTC()
		citation.PublishedAt = citation.PublishedAt.UTC()
		if existing, ok := byKey[citation.Key]; ok && existing != citation {
			return nil, nil, fmt.Errorf("person enrichment citation key %q has conflicting metadata", citation.Key)
		}
		byKey[citation.Key] = citation
	}
	result := make([]personenrichment.Citation, 0, len(byKey))
	keys := make(map[string]struct{}, len(byKey))
	for key, citation := range byKey {
		keys[key] = struct{}{}
		result = append(result, citation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, keys, nil
}

func validateEnrichmentSourceAttempts(input []personenrichment.SourceAttempt) error {
	seen := make(map[string]personenrichment.SourceAttempt, len(input))
	for i, source := range input {
		if !isCanonicalEnrichmentHTTPSURL(source.URL) || !validEnrichmentSourceOutcome(source.Outcome) ||
			source.ObservedAt.IsZero() || !enrichmentTimeIsUTC(source.ObservedAt) {
			return fmt.Errorf("person enrichment source attempt %d is invalid", i)
		}
		key := source.URL + "\x00" + source.Outcome
		if existing, ok := seen[key]; ok && !existing.ObservedAt.Equal(source.ObservedAt) {
			return fmt.Errorf("person enrichment source attempt %d conflicts with duplicate", i)
		}
		seen[key] = source
	}
	return nil
}

func translateEnrichmentClaims(
	claims []personfacts.ProposedClaim, citations []personenrichment.Citation,
	personID, attemptID int64, result personenrichment.Result, completionTime time.Time,
) ([]personfacts.ProposedClaim, error) {
	citationByKey := make(map[string]personenrichment.Citation, len(citations))
	for _, citation := range citations {
		citationByKey[citation.Key] = citation
	}
	translated, err := claimsWithIdentityScore(claims, 0)
	if err != nil {
		return nil, err
	}
	subject := personID
	aligner := externalEvidenceAligner{
		AttemptID: attemptID, ProviderRequestID: result.RequestID, ProviderJobID: result.JobID,
	}
	for i := range translated {
		for j := range translated[i].Evidence {
			evidence := &translated[i].Evidence[j]
			evidence.PersonID = personID
			evidence.SubjectPersonID = &subject
			evidence.SubjectRef = "person:" + strconv.FormatInt(personID, 10)
			evidence.SpanStart = nil
			evidence.SpanEnd = nil
			switch evidence.SourceClass {
			case personfacts.EvidencePublic:
				citation, ok := citationByKey[evidence.SourceRef]
				if !ok {
					if evidence.RecordedTime.IsZero() {
						evidence.RecordedTime = completionTime
					}
					if evidence.EventTime.IsZero() {
						evidence.EventTime = completionTime
					}
					continue
				}
				if evidence.SourceURL == "" {
					evidence.SourceURL = citation.URL
				}
				evidence.Excerpt = citation.Excerpt
				evidence.RecordedTime = citation.RetrievedAt
				evidence.EventTime = citation.PublishedAt
				if evidence.EventTime.IsZero() {
					evidence.EventTime = citation.RetrievedAt
				}
				evidence.SourceVersion = citation.Key
				evidence.ContentSHA256 = enrichmentEvidenceContentHash(*evidence)
			case personfacts.EvidenceProviderAssertion:
				if aligner.providerAssertionRef() == "" {
					return nil, fmt.Errorf("enrichment claim %d provider assertion has no opaque request or job ID", i)
				}
				if evidence.SourceRef != "" || evidence.SourceURL != "" {
					// Only the host owns the durable attempt ID. Adapters must not be
					// able to choose, predict, or replay local provenance coordinates
					// or attach a public URL to an uncited assertion. Leave this
					// deliberately unbound so the external aligner rejects it.
					evidence.SourceRef = ""
					continue
				}
				evidence.SourceRef = aligner.providerAssertionRef()
				evidence.SourceURL = ""
				evidence.RecordedTime = completionTime
				evidence.EventTime = result.FreshAsOf.UTC()
				if result.FreshAsOf.IsZero() {
					evidence.EventTime = completionTime
				}
				evidence.SourceVersion = enrichmentEvidenceVersion(*evidence)
				evidence.ContentSHA256 = enrichmentEvidenceContentHash(*evidence)
			default:
				translated[i].Origin = personfacts.ClaimOrigin("invalid-external-source")
			}
		}
	}
	return translated, nil
}

func citationURLs(citations []personenrichment.Citation) map[string]string {
	result := make(map[string]string, len(citations))
	for _, citation := range citations {
		result[citation.Key] = citation.URL
	}
	return result
}

func (s *Store) recheckPersonEnrichmentCatalogTx(
	ctx context.Context, tx *loggedTx, profile personenrichment.ProviderProfile,
) (personenrichment.ClaimOutcomeStatus, error) {
	catalog, err := s.buildPersonFactCatalogContext(ctx, tx, true)
	if err != nil {
		return "", err
	}
	byKey := make(map[string]personfacts.TargetDescriptor, len(catalog.Targets))
	for _, target := range catalog.Targets {
		byKey[target.Key] = target
	}
	current := make([]personfacts.TargetDescriptor, 0, len(profile.Targets))
	for _, target := range profile.Targets {
		actual, ok := byKey[target.Key]
		if !ok {
			return "", fmt.Errorf("%w: target catalog", errPersonEnrichmentResultEnvelopeChanged)
		}
		if actual.Sensitive && !profile.AllowSensitiveTargets {
			return personenrichment.ClaimPolicyRejected, nil
		}
		if !reflect.DeepEqual(actual, target) {
			return "", fmt.Errorf("%w: target descriptor", errPersonEnrichmentResultEnvelopeChanged)
		}
		current = append(current, actual)
	}
	fingerprint, err := personfacts.CatalogFingerprint(current)
	if err != nil {
		return "", err
	}
	if fingerprint != profile.CatalogFingerprint {
		return "", fmt.Errorf("%w: catalog fingerprint", errPersonEnrichmentResultEnvelopeChanged)
	}
	return personenrichment.ClaimApplied, nil
}

func personEnrichmentCitationMatches(
	stored PersonEnrichmentCitation, citation personenrichment.Citation,
) bool {
	if stored.CitationKey != citation.Key || stored.CanonicalURL != citation.URL ||
		stored.Title != citation.Title || stored.Publisher != citation.Publisher ||
		stored.Excerpt != citation.Excerpt || !stored.RetrievedAt.Equal(citation.RetrievedAt) {
		return false
	}
	if citation.PublishedAt.IsZero() {
		return stored.PublishedAt == nil
	}
	return stored.PublishedAt != nil && stored.PublishedAt.Equal(citation.PublishedAt)
}

func enrichmentCommitLeaseToken(
	commit personenrichment.ClaimCommit, owner string,
) personenrichment.LeaseToken {
	return personenrichment.LeaseToken{
		RunID: commit.RunID, WorkPersonID: commit.PersonID,
		ProfileFingerprint: commit.ProfileFingerprint, AttemptID: commit.AttemptID,
		Owner: owner, Fence: commit.LeaseFence,
	}
}

func enrichmentProviderIdentityLockKey(namespace, providerPersonID string) string {
	digest := sha256.Sum256([]byte(
		providerIdentityLockDomainV1 + "\x00" + namespace + "\x00" + providerPersonID))
	return hex.EncodeToString(digest[:])
}

func personEnrichmentSuppressionLockKey(
	namespace string, class personenrichment.SuppressionIdentifierClass,
	normalizationVersion, keyID string, digest []byte,
) string {
	data := []byte("msgvault/person-enrichment/suppression/v1\x00" + namespace + "\x00" +
		string(class) + "\x00" + normalizationVersion + "\x00" + keyID + "\x00")
	data = append(data, digest...)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func isCanonicalEnrichmentHTTPSURL(value string) bool {
	canonical, err := personenrichment.CanonicalPublicURL(value)
	if err != nil || canonical != value {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https"
}

func validEnrichmentSourceOutcome(value string) bool {
	switch value {
	case "cited", "visited", "failed", "blocked", "unsupported":
		return true
	default:
		return false
	}
}

func enrichmentEvidenceVersion(input personfacts.EvidenceInput) string {
	digest := sha256.Sum256([]byte(input.SourceRef + "\x00" + input.SourceURL))
	return "enrichment-v1:" + hex.EncodeToString(digest[:])
}

func enrichmentEvidenceContentHash(input personfacts.EvidenceInput) string {
	digest := sha256.Sum256([]byte(input.SourceRef + "\x00" + input.SourceURL + "\x00" + input.Excerpt))
	return hex.EncodeToString(digest[:])
}

func cloneEnrichmentDigests(
	input []personenrichment.SuppressionDigest,
) []personenrichment.SuppressionDigest {
	result := make([]personenrichment.SuppressionDigest, len(input))
	copy(result, input)
	for i := range result {
		result[i].Digest = slices.Clone(input[i].Digest)
	}
	return result
}

func nullOpaqueValue(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func nullableEnrichmentTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

func enrichmentTimeIsUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}

func copyEnrichmentTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func copyEnrichmentInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
