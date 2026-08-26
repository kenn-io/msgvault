package personfacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type preparedKeyedEvidence struct {
	key      string
	evidence EvidenceInput
}

// PreparePersonFactGeneration is the only path from submitted generation data
// to persistence-ready claims. It finishes archive alignment before returning
// and retains deterministic submitted-data failures in the prepared value.
func PreparePersonFactGeneration(
	ctx context.Context, input GenerationInput, aligner EvidenceAligner,
) (PreparedGeneration, error) {
	canonicalInput := copyGenerationInput(input)
	if err := validateGenerationInput(canonicalInput); err != nil {
		return PreparedGeneration{}, err
	}
	canonicalInput.ResolvedAt = portableFactTime(canonicalInput.ResolvedAt)
	sortSourceCursors(canonicalInput.SourceCursors)

	statusChanges, err := prepareEvidenceStatusChanges(canonicalInput.EvidenceStatusChanges)
	if err != nil {
		return PreparedGeneration{}, err
	}
	claims := make([]PreparedClaim, len(canonicalInput.Claims))
	for i := range canonicalInput.Claims {
		claims[i], err = prepareClaim(ctx, canonicalInput.PersonID, canonicalInput.Policy,
			canonicalInput.Claims[i], aligner)
		if err != nil {
			return PreparedGeneration{}, err
		}
		normalizeProposedClaimTimes(&canonicalInput.Claims[i])
	}
	sortPreparedClaims(claims)
	if err := rejectDuplicateCanonicalClaims(claims); err != nil {
		return PreparedGeneration{}, err
	}

	canonicalJSON, err := canonicalGenerationJSON(canonicalInput, claims, statusChanges)
	if err != nil {
		return PreparedGeneration{}, err
	}
	return PreparedGeneration{
		canonicalJSON:         append([]byte(nil), canonicalJSON...),
		generationKey:         fingerprint(canonicalJSON),
		programFingerprint:    canonicalInput.ProgramFingerprint,
		input:                 copyGenerationInput(canonicalInput),
		claims:                copyPreparedClaims(claims),
		evidenceStatusChanges: append([]PreparedEvidenceStatusChange(nil), statusChanges...),
	}, nil
}

func (p PreparedGeneration) CanonicalJSON() []byte {
	return append([]byte(nil), p.canonicalJSON...)
}

func (p PreparedGeneration) GenerationKey() string { return p.generationKey }

func (p PreparedGeneration) ProgramFingerprint() string { return p.programFingerprint }

func (p PreparedGeneration) Input() GenerationInput { return copyGenerationInput(p.input) }

func (p PreparedGeneration) Claims() []PreparedClaim { return copyPreparedClaims(p.claims) }

func (p PreparedGeneration) EvidenceStatusChanges() []PreparedEvidenceStatusChange {
	return append([]PreparedEvidenceStatusChange(nil), p.evidenceStatusChanges...)
}

func prepareClaim(
	ctx context.Context, personID int64, policy PolicyContext,
	claim ProposedClaim, aligner EvidenceAligner,
) (PreparedClaim, error) {
	prepared := PreparedClaim{
		Target: canonicalTarget(claim.Target), Relation: claim.Relation,
		SubmittedValue: append(json.RawMessage(nil), claim.SubmittedValue...),
		ValidFrom:      copyTimePointer(claim.ValidFrom), ValidUntil: copyTimePointer(claim.ValidUntil),
		Origin: claim.Origin, Confidence: claim.Confidence,
	}
	canonicalSubmitted, err := canonicalizeRawJSON(claim.SubmittedValue)
	if err != nil {
		prepared.SubmittedFingerprint = fingerprint(claim.SubmittedValue)
	} else {
		prepared.SubmittedFingerprint = fingerprint(canonicalSubmitted)
	}

	prepared.Normalized, prepared.Failure, err = NormalizeClaimValue(
		prepared.Target, claim.SubmittedValue)
	if err != nil {
		return PreparedClaim{}, err
	}
	vocabularyFailureDetail := invalidClaimVocabularyDetail(claim)
	if failure := validateClaimEnvelope(personID, policy, claim); prepared.Failure == nil {
		prepared.Failure = failure
	}
	if prepared.Failure == nil && supportedClaimTarget(prepared.Target) {
		revision, revisionErr := DescriptorRevision(prepared.Target)
		if revisionErr != nil {
			return PreparedClaim{}, revisionErr
		}
		if revision != prepared.Target.Revision {
			prepared.Normalized = nil
			prepared.Failure = &ValidationFailure{
				Action: DecisionInvalid, Reason: ReasonStaleTargetRevision,
				Detail: "claim target descriptor content does not match its revision",
			}
		}
	}
	if vocabularyFailureDetail != "" && prepared.Failure != nil &&
		prepared.Failure.Detail != vocabularyFailureDetail {
		prepared.Failure.Detail += "; " + vocabularyFailureDetail
	}
	if claim.Relation != RelationSupport && claim.Relation != RelationContradict &&
		claim.Relation != RelationSupersede {
		prepared.Relation = RelationInvalid
	}
	if claim.Origin != OriginExtraction && claim.Origin != OriginEnrichment && claim.Origin != OriginSystem {
		prepared.Origin = OriginInvalid
	}
	prepared.ValidFrom = portableFactTimePointer(prepared.ValidFrom)
	prepared.ValidUntil = portableFactTimePointer(prepared.ValidUntil)

	prepared.SubmittedEvidenceFingerprints = make([]string, 0, len(claim.Evidence))
	keyed := make([]preparedKeyedEvidence, 0, len(claim.Evidence))
	for _, submittedEvidence := range claim.Evidence {
		submittedFingerprint, fingerprintErr := submittedEvidenceFingerprint(submittedEvidence)
		if fingerprintErr != nil {
			return PreparedClaim{}, fingerprintErr
		}
		prepared.SubmittedEvidenceFingerprints = append(
			prepared.SubmittedEvidenceFingerprints, submittedFingerprint)
		evidence := copyEvidenceInput(submittedEvidence)
		if evidence.PersonID != personID {
			if prepared.Failure == nil {
				prepared.Failure = &ValidationFailure{
					Action: DecisionIdentityRejected, Reason: ReasonIdentityMismatch,
					Detail: "evidence person id does not match generation person id",
				}
			}
			continue
		}
		evidence.EventTime = portableFactTime(evidence.EventTime)
		evidence.RecordedTime = portableFactTime(evidence.RecordedTime)
		alignExternal := claim.Origin == OriginEnrichment && aligner != nil &&
			(evidence.SourceClass == EvidencePublic || evidence.SourceClass == EvidenceProviderAssertion)
		if alignExternal {
			result, alignErr := aligner.Align(ctx, copyEvidenceInput(evidence))
			if alignErr != nil {
				return PreparedClaim{}, fmt.Errorf("align evidence %q: %w", evidence.SourceRef, alignErr)
			}
			if !result.Accepted {
				failure := result.Failure
				if failure == nil {
					failure = &ValidationFailure{
						Action: DecisionInvalid, Reason: ReasonUnalignedEvidence,
						Detail: "external evidence could not be aligned",
					}
				}
				if !validValidationFailure(failure) {
					return PreparedClaim{}, errors.New("aligner returned a failure outside the closed vocabulary")
				}
				if prepared.Failure == nil {
					prepared.Failure = copyValidationFailure(failure)
				}
				continue
			}
			if result.Failure != nil {
				return PreparedClaim{}, errors.New("accepted alignment result must not contain a failure")
			}
			evidence.SourceVersion = result.SourceVersion
			evidence.ContentSHA256 = result.ContentSHA256
			if validationErr := validateEvidenceInput(evidence); validationErr != nil {
				if prepared.Failure == nil {
					prepared.Failure = evidenceFailure(validationErr)
				}
				continue
			}
		} else {
			if validationErr := validateEvidenceInput(evidence); validationErr != nil {
				if prepared.Failure == nil {
					prepared.Failure = evidenceFailure(validationErr)
				}
				continue
			}
		}
		if evidence.SourceClass == EvidenceArchive {
			if aligner == nil {
				return PreparedClaim{}, errors.New("archive evidence aligner is required")
			}
			result, alignErr := aligner.Align(ctx, copyEvidenceInput(evidence))
			if alignErr != nil {
				return PreparedClaim{}, fmt.Errorf("align evidence %q: %w", evidence.SourceRef, alignErr)
			}
			if !result.Accepted {
				failure := result.Failure
				if failure == nil {
					failure = &ValidationFailure{
						Action: DecisionInvalid, Reason: ReasonUnalignedEvidence,
						Detail: "archive evidence could not be aligned",
					}
				}
				if !validValidationFailure(failure) {
					return PreparedClaim{}, errors.New("aligner returned a failure outside the closed vocabulary")
				}
				if prepared.Failure == nil {
					prepared.Failure = copyValidationFailure(failure)
				}
				continue
			}
			if result.Failure != nil {
				return PreparedClaim{}, errors.New("accepted alignment result must not contain a failure")
			}
			evidence.SourceVersion = result.SourceVersion
			evidence.ContentSHA256 = result.ContentSHA256
			if validationErr := validateEvidenceInput(evidence); validationErr != nil {
				return PreparedClaim{}, fmt.Errorf("aligner returned invalid immutable evidence: %w", validationErr)
			}
		}
		key, keyErr := EvidenceKey(evidence)
		if keyErr != nil {
			return PreparedClaim{}, fmt.Errorf("key validated evidence: %w", keyErr)
		}
		keyed = append(keyed, preparedKeyedEvidence{key: key, evidence: evidence})
	}
	sort.Strings(prepared.SubmittedEvidenceFingerprints)
	sort.Slice(keyed, func(i, j int) bool { return keyed[i].key < keyed[j].key })
	for i := 1; i < len(keyed); i++ {
		if keyed[i-1].key == keyed[i].key {
			return PreparedClaim{}, fmt.Errorf("duplicate evidence key %q in one claim", keyed[i].key)
		}
	}
	prepared.Evidence = make([]EvidenceInput, len(keyed))
	prepared.EvidenceKeys = make([]string, 0, len(keyed))
	for i := range keyed {
		prepared.Evidence[i] = copyEvidenceInput(keyed[i].evidence)
		if keyed[i].key != "" {
			prepared.EvidenceKeys = append(prepared.EvidenceKeys, keyed[i].key)
		}
	}
	return prepared, nil
}

func submittedEvidenceFingerprint(input EvidenceInput) (string, error) {
	type identity struct {
		PersonID        int64               `json:"person_id"`
		SourceClass     EvidenceSourceClass `json:"source_class"`
		Directness      EvidenceDirectness  `json:"directness"`
		Authority       EvidenceAuthority   `json:"authority"`
		SourceRef       string              `json:"source_ref"`
		SourceURL       string              `json:"source_url"`
		SubjectPersonID *int64              `json:"subject_person_id"`
		SubjectRef      string              `json:"subject_ref"`
		SpanStart       *int64              `json:"span_start"`
		SpanEnd         *int64              `json:"span_end"`
		Excerpt         string              `json:"excerpt"`
		ContentSHA256   string              `json:"content_sha256"`
		SourceVersion   string              `json:"source_version"`
		EventTime       string              `json:"event_time"`
		RecordedTime    string              `json:"recorded_time"`
		IdentityScore   int                 `json:"identity_score"`
	}
	encoded, err := json.Marshal(identity{
		PersonID: input.PersonID, SourceClass: input.SourceClass,
		Directness: input.Directness, Authority: input.Authority,
		SourceRef: input.SourceRef, SourceURL: input.SourceURL,
		SubjectPersonID: copyInt64Pointer(input.SubjectPersonID), SubjectRef: input.SubjectRef,
		SpanStart: copyInt64Pointer(input.SpanStart), SpanEnd: copyInt64Pointer(input.SpanEnd),
		Excerpt: input.Excerpt, ContentSHA256: input.ContentSHA256,
		SourceVersion: input.SourceVersion,
		EventTime:     portableFactTime(input.EventTime).Format(time.RFC3339Nano),
		RecordedTime:  portableFactTime(input.RecordedTime).Format(time.RFC3339Nano),
		IdentityScore: input.IdentityScore,
	})
	if err != nil {
		return "", fmt.Errorf("encode submitted evidence identity: %w", err)
	}
	return fingerprint(encoded), nil
}

func supportedClaimTarget(target TargetDescriptor) bool {
	return target.Kind == TargetEmployment && target.ValueType == ValueEmployment ||
		target.Kind == TargetAttribute && supportedGenericValueType(target.ValueType)
}

func validateClaimEnvelope(
	personID int64, policy PolicyContext, claim ProposedClaim,
) *ValidationFailure {
	invalid := func(action DecisionAction, reason DecisionReason, detail string) *ValidationFailure {
		return &ValidationFailure{Action: action, Reason: reason, Detail: detail}
	}
	if personID <= 0 {
		return invalid(DecisionInvalid, ReasonMalformedValue, "generation person id must be positive")
	}
	if claim.Target.Key == "" || claim.Target.Revision == "" {
		return invalid(DecisionInvalid, ReasonUnsupportedTarget, "claim target key and revision are required")
	}
	if detail := invalidClaimVocabularyDetail(claim); detail != "" {
		return invalid(DecisionInvalid, ReasonMalformedValue, detail)
	}
	if claim.Confidence.ReportedScore < 0 || claim.Confidence.ReportedScore > 1000 {
		return invalid(DecisionInvalid, ReasonMalformedValue, "reported confidence must be between 0 and 1000")
	}
	if claim.ValidFrom != nil && (!isUTC(*claim.ValidFrom) || claim.ValidFrom.IsZero()) {
		return invalid(DecisionInvalid, ReasonMalformedValue, "valid_from must be nonzero UTC")
	}
	if claim.ValidUntil != nil && (!isUTC(*claim.ValidUntil) || claim.ValidUntil.IsZero()) {
		return invalid(DecisionInvalid, ReasonMalformedValue, "valid_until must be nonzero UTC")
	}
	if claim.ValidFrom != nil && claim.ValidUntil != nil && claim.ValidUntil.Before(*claim.ValidFrom) {
		return invalid(DecisionInvalid, ReasonMalformedValue, "valid_until must not precede valid_from")
	}
	if claim.Target.Sensitive && !policy.AllowSensitive {
		return invalid(DecisionPolicyRejected, ReasonSensitivePolicy, "sensitive target is disabled by policy")
	}
	return nil
}

func invalidClaimVocabularyDetail(claim ProposedClaim) string {
	details := make([]string, 0, 2)
	if claim.Relation != RelationSupport && claim.Relation != RelationContradict &&
		claim.Relation != RelationSupersede {
		details = append(details, fmt.Sprintf("relation %q", claim.Relation))
	}
	if claim.Origin != OriginExtraction && claim.Origin != OriginEnrichment && claim.Origin != OriginSystem {
		details = append(details, fmt.Sprintf("origin %q", claim.Origin))
	}
	if len(details) == 0 {
		return ""
	}
	return "claim " + strings.Join(details, " and ") + " is not in the closed vocabulary"
}

func validateGenerationInput(input GenerationInput) error {
	if input.PersonID <= 0 {
		return errors.New("generation person id must be positive")
	}
	if len(input.SourceCursors) == 0 {
		return errors.New("generation requires at least one source cursor")
	}
	seen := make(map[string]struct{}, len(input.SourceCursors))
	for _, cursor := range input.SourceCursors {
		if cursor.Lane == "" || cursor.Start == "" || cursor.End == "" {
			return errors.New("source cursor lane, start, and end are required")
		}
		key := cursor.Lane + "\x00" + cursor.Start + "\x00" + cursor.End
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate source cursor tuple %q/%q/%q", cursor.Lane, cursor.Start, cursor.End)
		}
		seen[key] = struct{}{}
	}
	if input.ProgramID == "" || input.ProgramVersion == "" {
		return errors.New("generation program id and version are required")
	}
	if !lowercaseSHA256Pattern.MatchString(input.ProgramFingerprint) {
		return errors.New("generation program fingerprint must be lowercase 64-hex SHA-256")
	}
	if input.CatalogFingerprint == "" {
		return errors.New("generation catalog fingerprint is required")
	}
	if input.Provider == "" || input.ProviderVersion == "" {
		return errors.New("generation provider and provider version are required")
	}
	if input.ResolvedAt.IsZero() || !isUTC(input.ResolvedAt) {
		return errors.New("generation resolution time must be nonzero UTC")
	}
	return nil
}

func prepareEvidenceStatusChanges(
	changes []EvidenceStatusChange,
) ([]PreparedEvidenceStatusChange, error) {
	prepared := make([]PreparedEvidenceStatusChange, len(changes))
	seen := make(map[string]struct{}, len(changes))
	for i, change := range changes {
		if change.EvidenceKey == "" {
			return nil, errors.New("evidence status key is required")
		}
		if !validEvidenceStatusTransition(change.Supported, change.Reason) {
			return nil, fmt.Errorf("evidence status reason %q is invalid for supported=%t", change.Reason, change.Supported)
		}
		key := change.EvidenceKey + "\x00" + change.SourceVersion
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate evidence status tuple for %q at %q",
				change.EvidenceKey, change.SourceVersion)
		}
		seen[key] = struct{}{}
		prepared[i] = PreparedEvidenceStatusChange(change)
	}
	sort.Slice(prepared, func(i, j int) bool {
		if prepared[i].EvidenceKey != prepared[j].EvidenceKey {
			return prepared[i].EvidenceKey < prepared[j].EvidenceKey
		}
		if prepared[i].SourceVersion != prepared[j].SourceVersion {
			return prepared[i].SourceVersion < prepared[j].SourceVersion
		}
		return false
	})
	return prepared, nil
}

func validEvidenceStatusTransition(supported bool, reason EvidenceStatusReason) bool {
	if supported {
		return reason == EvidenceStatusSourceReimported || reason == EvidenceStatusScopeRelinked
	}
	return reason == EvidenceStatusSourceDeleted || reason == EvidenceStatusSourceEdited ||
		reason == EvidenceStatusScopeUnlinked || reason == EvidenceStatusIdentityReassigned
}

// GenerationKey hashes the canonical production envelope. ResolvedAt is
// intentionally excluded and is stored separately with first-writer semantics.
func GenerationKey(
	input GenerationInput, claims []PreparedClaim, statusChanges []PreparedEvidenceStatusChange,
) (string, error) {
	canonical, err := canonicalGenerationJSON(copyGenerationInput(input), copyPreparedClaims(claims),
		append([]PreparedEvidenceStatusChange(nil), statusChanges...))
	if err != nil {
		return "", err
	}
	return fingerprint(canonical), nil
}

// ClaimKey hashes the canonical claim identity within one generation.
func ClaimKey(generationKey string, prepared PreparedClaim) (string, error) {
	if generationKey == "" {
		return "", errors.New("generation key is required")
	}
	view := canonicalClaimView(prepared)
	encoded, err := json.Marshal(struct {
		GenerationKey string         `json:"generation_key"`
		Claim         canonicalClaim `json:"claim"`
	}{GenerationKey: generationKey, Claim: view})
	if err != nil {
		return "", fmt.Errorf("encode claim key: %w", err)
	}
	return fingerprint(encoded), nil
}

// DecisionKey binds a final decision action to one resolution and claim.
func DecisionKey(resolutionFingerprint, claimKey string, action DecisionAction) (string, error) {
	if resolutionFingerprint == "" || claimKey == "" {
		return "", errors.New("resolution fingerprint and claim key are required")
	}
	if !validDecisionAction(action) {
		return "", fmt.Errorf("unknown decision action %q", action)
	}
	encoded, err := json.Marshal(struct {
		ResolutionFingerprint string         `json:"resolution_fingerprint"`
		ClaimKey              string         `json:"claim_key"`
		Action                DecisionAction `json:"action"`
	}{resolutionFingerprint, claimKey, action})
	if err != nil {
		return "", fmt.Errorf("encode decision key: %w", err)
	}
	return fingerprint(encoded), nil
}

// ResolutionInputFingerprint hashes a canonical copy of resolver input. Task
// 3 will consume this stable input-side seam when it adds resolver behavior.
func ResolutionInputFingerprint(input ResolutionInput) (string, error) {
	cloned := copyResolutionInput(input)
	normalizeResolutionInputFactTimes(&cloned)
	sort.Slice(cloned.Current, func(i, j int) bool {
		if cloned.Current[i].Ref.Kind != cloned.Current[j].Ref.Kind {
			return cloned.Current[i].Ref.Kind < cloned.Current[j].Ref.Kind
		}
		if cloned.Current[i].Ref.RowID != cloned.Current[j].Ref.RowID {
			return cloned.Current[i].Ref.RowID < cloned.Current[j].Ref.RowID
		}
		return cloned.Current[i].Normalized.Fingerprint < cloned.Current[j].Normalized.Fingerprint
	})
	for i := range cloned.Claims {
		sort.Slice(cloned.Claims[i].Evidence, func(a, b int) bool {
			return evidenceSortKey(cloned.Claims[i].Evidence[a]) < evidenceSortKey(cloned.Claims[i].Evidence[b])
		})
	}
	sort.Slice(cloned.Claims, func(i, j int) bool {
		if cloned.Claims[i].Claim.Target.Key != cloned.Claims[j].Claim.Target.Key {
			return cloned.Claims[i].Claim.Target.Key < cloned.Claims[j].Claim.Target.Key
		}
		if normalizedFingerprint(cloned.Claims[i].Normalized) != normalizedFingerprint(cloned.Claims[j].Normalized) {
			return normalizedFingerprint(cloned.Claims[i].Normalized) < normalizedFingerprint(cloned.Claims[j].Normalized)
		}
		return cloned.Claims[i].ClaimKey < cloned.Claims[j].ClaimKey
	})
	encoded, err := json.Marshal(cloned)
	if err != nil {
		return "", fmt.Errorf("encode resolution input fingerprint: %w", err)
	}
	return fingerprint(encoded), nil
}

func normalizeResolutionInputFactTimes(input *ResolutionInput) {
	input.ResolvedAt = portableFactTime(input.ResolvedAt)
	for i := range input.Current {
		input.Current[i].ActiveFrom = portableFactTime(input.Current[i].ActiveFrom)
		input.Current[i].ActiveUntil = portableFactTimePointer(input.Current[i].ActiveUntil)
		input.Current[i].TransactionTime = portableFactTime(input.Current[i].TransactionTime)
	}
	for i := range input.Claims {
		normalizeProposedClaimTimes(&input.Claims[i].Claim)
		for j := range input.Claims[i].Evidence {
			input.Claims[i].Evidence[j].Input.EventTime =
				portableFactTime(input.Claims[i].Evidence[j].Input.EventTime)
			input.Claims[i].Evidence[j].Input.RecordedTime =
				portableFactTime(input.Claims[i].Evidence[j].Input.RecordedTime)
		}
	}
}

func validDecisionAction(action DecisionAction) bool {
	switch action {
	case DecisionApplied, DecisionRetained, DecisionSuperseded, DecisionInvalid,
		DecisionIdentityRejected, DecisionPolicyRejected, DecisionConflictRejected,
		DecisionAmbiguousRetained:
		return true
	default:
		return false
	}
}

func validValidationFailure(failure *ValidationFailure) bool {
	if failure == nil || !validDecisionAction(failure.Action) || failure.Detail == "" {
		return false
	}
	switch failure.Reason {
	case ReasonMalformedValue, ReasonUnsupportedTarget, ReasonStaleTargetRevision,
		ReasonUnalignedEvidence, ReasonIdentityMismatch, ReasonSensitivePolicy,
		ReasonPinRetained, ReasonBelowThreshold, ReasonInsufficientMargin,
		ReasonCompetingTie, ReasonExplicitContradiction, ReasonExplicitSupersession,
		ReasonOrganizationAmbiguous, ReasonAppliedProjection, ReasonEvidenceUnsupported:
		return true
	default:
		return false
	}
}

type canonicalNormalized struct {
	JSON        json.RawMessage `json:"json"`
	Fingerprint string          `json:"fingerprint"`
}

type canonicalFailure struct {
	Action DecisionAction `json:"action"`
	Reason DecisionReason `json:"reason"`
	Detail string         `json:"detail"`
}

type canonicalClaim struct {
	Target                        TargetRef            `json:"target"`
	Relation                      ClaimRelation        `json:"relation"`
	SubmittedFingerprint          string               `json:"submitted_fingerprint"`
	SubmittedEvidenceFingerprints []string             `json:"submitted_evidence_fingerprints"`
	Normalized                    *canonicalNormalized `json:"normalized"`
	EvidenceKeys                  []string             `json:"evidence_keys"`
	ValidFrom                     *string              `json:"valid_from"`
	ValidUntil                    *string              `json:"valid_until"`
	Origin                        ClaimOrigin          `json:"origin"`
	Confidence                    ConfidenceInputs     `json:"confidence"`
	Failure                       *canonicalFailure    `json:"failure"`
}

type canonicalGeneration struct {
	PersonID           int64             `json:"person_id"`
	SourceCursors      []SourceCursor    `json:"source_cursors"`
	ProgramID          string            `json:"program_id"`
	ProgramVersion     string            `json:"program_version"`
	ProgramFingerprint string            `json:"program_fingerprint"`
	CatalogFingerprint string            `json:"catalog_fingerprint"`
	Provider           string            `json:"provider"`
	ProviderVersion    string            `json:"provider_version"`
	Model              string            `json:"model"`
	ModelVersion       string            `json:"model_version"`
	Policy             PolicyContext     `json:"policy"`
	Claims             []canonicalClaim  `json:"claims"`
	StatusChanges      []canonicalStatus `json:"evidence_status_changes"`
}

type canonicalStatus struct {
	EvidenceKey   string               `json:"evidence_key"`
	SourceVersion string               `json:"source_version"`
	Supported     bool                 `json:"supported"`
	Reason        EvidenceStatusReason `json:"reason"`
}

func canonicalGenerationJSON(
	input GenerationInput, claims []PreparedClaim, statusChanges []PreparedEvidenceStatusChange,
) ([]byte, error) {
	if err := validateGenerationInput(input); err != nil {
		return nil, err
	}
	sortSourceCursors(input.SourceCursors)
	statusInput := make([]EvidenceStatusChange, len(statusChanges))
	for i := range statusChanges {
		statusInput[i] = EvidenceStatusChange(statusChanges[i])
	}
	canonicalStatuses, err := prepareEvidenceStatusChanges(statusInput)
	if err != nil {
		return nil, err
	}
	sortPreparedClaims(claims)
	claimViews := make([]canonicalClaim, len(claims))
	for i := range claims {
		claimViews[i] = canonicalClaimView(claims[i])
	}
	statusViews := make([]canonicalStatus, len(canonicalStatuses))
	for i := range canonicalStatuses {
		statusViews[i] = canonicalStatus{
			EvidenceKey:   canonicalStatuses[i].EvidenceKey,
			SourceVersion: canonicalStatuses[i].SourceVersion,
			Supported:     canonicalStatuses[i].Supported,
			Reason:        canonicalStatuses[i].Reason,
		}
	}
	return json.Marshal(canonicalGeneration{
		PersonID: input.PersonID, SourceCursors: input.SourceCursors,
		ProgramID: input.ProgramID, ProgramVersion: input.ProgramVersion,
		ProgramFingerprint: input.ProgramFingerprint, CatalogFingerprint: input.CatalogFingerprint,
		Provider: input.Provider, ProviderVersion: input.ProviderVersion,
		Model: input.Model, ModelVersion: input.ModelVersion, Policy: input.Policy,
		Claims: claimViews, StatusChanges: statusViews,
	})
}

func canonicalClaimView(prepared PreparedClaim) canonicalClaim {
	submittedEvidenceFingerprints := append(
		[]string(nil), prepared.SubmittedEvidenceFingerprints...)
	sort.Strings(submittedEvidenceFingerprints)
	if submittedEvidenceFingerprints == nil {
		submittedEvidenceFingerprints = []string{}
	}
	evidenceKeys := append([]string(nil), prepared.EvidenceKeys...)
	sort.Strings(evidenceKeys)
	if evidenceKeys == nil {
		evidenceKeys = []string{}
	}
	var normalized *canonicalNormalized
	if prepared.Normalized != nil {
		normalized = &canonicalNormalized{
			JSON:        append(json.RawMessage(nil), prepared.Normalized.JSON...),
			Fingerprint: prepared.Normalized.Fingerprint,
		}
	}
	var failure *canonicalFailure
	if prepared.Failure != nil {
		failure = &canonicalFailure{
			Action: prepared.Failure.Action, Reason: prepared.Failure.Reason, Detail: prepared.Failure.Detail,
		}
	}
	return canonicalClaim{
		Target:   TargetRef{Kind: prepared.Target.Kind, Key: prepared.Target.Key, Revision: prepared.Target.Revision},
		Relation: prepared.Relation, SubmittedFingerprint: prepared.SubmittedFingerprint,
		SubmittedEvidenceFingerprints: submittedEvidenceFingerprints,
		Normalized:                    normalized, EvidenceKeys: evidenceKeys,
		ValidFrom: canonicalTimePointer(prepared.ValidFrom), ValidUntil: canonicalTimePointer(prepared.ValidUntil),
		Origin: prepared.Origin, Confidence: prepared.Confidence, Failure: failure,
	}
}

func sortPreparedClaims(claims []PreparedClaim) {
	sort.Slice(claims, func(i, j int) bool {
		left, _ := json.Marshal(canonicalClaimView(claims[i]))
		right, _ := json.Marshal(canonicalClaimView(claims[j]))
		return string(left) < string(right)
	})
}

func rejectDuplicateCanonicalClaims(claims []PreparedClaim) error {
	var previous string
	for i := range claims {
		encoded, err := json.Marshal(canonicalClaimView(claims[i]))
		if err != nil {
			return fmt.Errorf("encode canonical claim for duplicate validation: %w", err)
		}
		current := string(encoded)
		if i > 0 && current == previous {
			return errors.New("duplicate canonical claim in generation")
		}
		previous = current
	}
	return nil
}

func sortSourceCursors(cursors []SourceCursor) {
	sort.Slice(cursors, func(i, j int) bool {
		if cursors[i].Lane != cursors[j].Lane {
			return cursors[i].Lane < cursors[j].Lane
		}
		if cursors[i].Start != cursors[j].Start {
			return cursors[i].Start < cursors[j].Start
		}
		return cursors[i].End < cursors[j].End
	})
}

func copyGenerationInput(input GenerationInput) GenerationInput {
	cloned := input
	cloned.SourceCursors = append([]SourceCursor(nil), input.SourceCursors...)
	cloned.Claims = make([]ProposedClaim, len(input.Claims))
	for i := range input.Claims {
		cloned.Claims[i] = copyProposedClaim(input.Claims[i])
	}
	cloned.EvidenceStatusChanges = append([]EvidenceStatusChange(nil), input.EvidenceStatusChanges...)
	return cloned
}

func copyProposedClaim(input ProposedClaim) ProposedClaim {
	cloned := input
	cloned.Target = canonicalTarget(input.Target)
	cloned.SubmittedValue = append(json.RawMessage(nil), input.SubmittedValue...)
	cloned.Evidence = make([]EvidenceInput, len(input.Evidence))
	for i := range input.Evidence {
		cloned.Evidence[i] = copyEvidenceInput(input.Evidence[i])
	}
	cloned.ValidFrom = copyTimePointer(input.ValidFrom)
	cloned.ValidUntil = copyTimePointer(input.ValidUntil)
	return cloned
}

func copyEvidenceInput(input EvidenceInput) EvidenceInput {
	cloned := input
	cloned.SubjectPersonID = copyInt64Pointer(input.SubjectPersonID)
	cloned.SpanStart = copyInt64Pointer(input.SpanStart)
	cloned.SpanEnd = copyInt64Pointer(input.SpanEnd)
	return cloned
}

func copyPreparedClaims(input []PreparedClaim) []PreparedClaim {
	cloned := make([]PreparedClaim, len(input))
	for i := range input {
		cloned[i] = copyPreparedClaim(input[i])
	}
	return cloned
}

func copyPreparedClaim(input PreparedClaim) PreparedClaim {
	cloned := input
	cloned.Target = canonicalTarget(input.Target)
	cloned.SubmittedValue = append(json.RawMessage(nil), input.SubmittedValue...)
	cloned.SubmittedEvidenceFingerprints = append(
		[]string(nil), input.SubmittedEvidenceFingerprints...)
	if input.Normalized != nil {
		cloned.Normalized = &NormalizedValue{
			JSON: append(json.RawMessage(nil), input.Normalized.JSON...), Fingerprint: input.Normalized.Fingerprint,
		}
	}
	cloned.Evidence = make([]EvidenceInput, len(input.Evidence))
	for i := range input.Evidence {
		cloned.Evidence[i] = copyEvidenceInput(input.Evidence[i])
	}
	cloned.EvidenceKeys = append([]string(nil), input.EvidenceKeys...)
	cloned.ValidFrom = copyTimePointer(input.ValidFrom)
	cloned.ValidUntil = copyTimePointer(input.ValidUntil)
	cloned.Failure = copyValidationFailure(input.Failure)
	return cloned
}

func copyResolutionInput(input ResolutionInput) ResolutionInput {
	cloned := input
	cloned.Target = canonicalTarget(input.Target)
	cloned.Current = append([]CurrentProjection(nil), input.Current...)
	for i := range cloned.Current {
		cloned.Current[i].Normalized.JSON = append(json.RawMessage(nil), input.Current[i].Normalized.JSON...)
		cloned.Current[i].ActiveUntil = copyTimePointer(input.Current[i].ActiveUntil)
	}
	cloned.Claims = append([]ResolvedClaim(nil), input.Claims...)
	for i := range cloned.Claims {
		cloned.Claims[i].Claim = copyProposedClaim(input.Claims[i].Claim)
		if input.Claims[i].Normalized != nil {
			cloned.Claims[i].Normalized = &NormalizedValue{
				JSON:        append(json.RawMessage(nil), input.Claims[i].Normalized.JSON...),
				Fingerprint: input.Claims[i].Normalized.Fingerprint,
			}
		}
		cloned.Claims[i].Evidence = append([]Evidence(nil), input.Claims[i].Evidence...)
		for j := range cloned.Claims[i].Evidence {
			cloned.Claims[i].Evidence[j].Input = copyEvidenceInput(input.Claims[i].Evidence[j].Input)
			if input.Claims[i].Evidence[j].LatestStatus != nil {
				status := *input.Claims[i].Evidence[j].LatestStatus
				cloned.Claims[i].Evidence[j].LatestStatus = &status
			}
		}
		cloned.Claims[i].Failure = copyValidationFailure(input.Claims[i].Failure)
	}
	cloned.Pin.EventID = copyInt64Pointer(input.Pin.EventID)
	return cloned
}

func copyInt64Pointer(input *int64) *int64 {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func copyTimePointer(input *time.Time) *time.Time {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func copyValidationFailure(input *ValidationFailure) *ValidationFailure {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func canonicalTimePointer(input *time.Time) *string {
	if input == nil {
		return nil
	}
	value := portableFactTime(*input).Format(time.RFC3339Nano)
	return &value
}

func normalizeProposedClaimTimes(claim *ProposedClaim) {
	claim.ValidFrom = portableFactTimePointer(claim.ValidFrom)
	claim.ValidUntil = portableFactTimePointer(claim.ValidUntil)
	for i := range claim.Evidence {
		claim.Evidence[i].EventTime = portableFactTime(claim.Evidence[i].EventTime)
		claim.Evidence[i].RecordedTime = portableFactTime(claim.Evidence[i].RecordedTime)
	}
}

func portableFactTime(input time.Time) time.Time {
	return input.UTC().Truncate(time.Microsecond)
}

func portableFactTimePointer(input *time.Time) *time.Time {
	if input == nil {
		return nil
	}
	value := portableFactTime(*input)
	return &value
}

func evidenceSortKey(evidence Evidence) string {
	if evidence.Key != "" {
		return evidence.Key
	}
	key, err := EvidenceKey(evidence.Input)
	if err == nil {
		return key
	}
	encoded, err := json.Marshal(evidenceKeyView(evidence.Input))
	if err != nil {
		return ""
	}
	return fingerprint(encoded)
}

func normalizedFingerprint(value *NormalizedValue) string {
	if value == nil {
		return ""
	}
	return value.Fingerprint
}
