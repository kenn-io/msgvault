package store

import (
	"context"
	"fmt"
	"strings"
)

// Pair explanations use a separate deduplication key from independent producer
// explanations. Candidate refs record creation only, never exclusive ownership:
// later independent evidence survives corroborating upserts and participant merges.
const observationPairRefPrefix = "observation-pair:"
const observationPairEmailRef = observationPairRefPrefix + "email"

// The existing Contacts review producer establishes an independent claim using
// this evidence kind. Untyped source associations and nil refs alone do not:
// old observation-pair producers also wrote nil refs and flat source unions.
// sourceSQL, when supplied, restricts proof to that actual contributing source.
func independentContactMatchSupportSQL(sourceSQL string) string {
	source := ""
	if sourceSQL != "" {
		source = " AND independent_source.source_id=" + sourceSQL
	}
	return `EXISTS (SELECT 1 FROM identity_match_evidence independent_evidence
 JOIN identity_match_evidence_sources independent_source ON independent_source.evidence_id=independent_evidence.id
 JOIN sources independent_archive ON independent_archive.id=independent_source.source_id
 WHERE independent_evidence.candidate_id=c.id
 AND independent_evidence.evidence_kind='muesli_ambiguous_contact'
 AND independent_evidence.source='archive_observation'
 AND independent_archive.source_type='muesli'` + source + `)`
}

func independentEmailEvidenceSupportSQL() string {
	return `(COALESCE(identity_match_evidence.evidence_ref,'') NOT LIKE '` + observationPairRefPrefix + `%'
 AND EXISTS (SELECT 1 FROM identity_match_evidence_sources independent_email_source
 WHERE independent_email_source.evidence_id=identity_match_evidence.id
 AND ` + independentContactMatchSupportSQL("independent_email_source.source_id") + `))`
}

func candidateObservationPairForSourceSQL(sourceSQL string) string {
	return strings.Replace(currentCandidateObservationPairSQL,
		"WHERE observation_left.participant_id = c.left_id",
		"WHERE (observation_left.source_id="+sourceSQL+" OR observation_right.source_id="+sourceSQL+") AND observation_left.participant_id = c.left_id", 1)
}

func emailEvidencePairForSourceSQL(sourceSQL string) string {
	return `EXISTS (SELECT 1 FROM participant_contact_observations observation_left
 JOIN participant_contact_observations observation_right ON TRUE
 WHERE observation_left.participant_id=c.left_id AND observation_right.participant_id=c.right_id
 AND (observation_left.source_id=` + sourceSQL + ` OR observation_right.source_id=` + sourceSQL + `)
 AND observation_left.active_until IS NULL AND observation_left.superseded_at IS NULL
 AND observation_right.active_until IS NULL AND observation_right.superseded_at IS NULL
 AND ` + FullObservationWhere("observation_left") + ` AND ` + FullObservationWhere("observation_right") + `
 AND observation_left.address_kind=identity_match_evidence.evidence_kind
 AND observation_right.address_kind=identity_match_evidence.evidence_kind
 AND observation_left.normalized_value=identity_match_evidence.detail
 AND observation_right.normalized_value=identity_match_evidence.detail)`
}

// Preserve independent claims while withdrawing invalid paired contributions.
// Run before row cleanup, while immutable producer evidence is still available.
func pruneIndependentMatchCorroboration(ctx context.Context, exec sourceIdentityExec) error {
	if _, err := exec(ctx, `DELETE FROM identity_match_candidate_sources
 WHERE EXISTS (SELECT 1 FROM identity_match_candidates c
 WHERE c.id=identity_match_candidate_sources.candidate_id
 AND c.source IN ('archive_observation','extraction','enrichment')
 AND c.left_kind='participant' AND c.right_kind='participant'
 AND c.basis IN ('email','phone')
 AND `+independentContactMatchSupportSQL("")+`
 AND NOT `+independentContactMatchSupportSQL("identity_match_candidate_sources.source_id")+`
 AND NOT `+candidateObservationPairForSourceSQL("identity_match_candidate_sources.source_id")+`)`); err != nil {
		return fmt.Errorf("prune unsupported independent-match corroboration: %w", err)
	}
	if _, err := exec(ctx, `DELETE FROM identity_match_evidence_sources
 WHERE EXISTS (SELECT 1 FROM identity_match_evidence JOIN identity_match_candidates c ON c.id=identity_match_evidence.candidate_id
 WHERE identity_match_evidence.id=identity_match_evidence_sources.evidence_id
 AND identity_match_evidence.source IN ('archive_observation','extraction','enrichment')
 AND identity_match_evidence.evidence_kind IN ('email','phone')
 AND `+independentEmailEvidenceSupportSQL()+`
 AND NOT `+independentContactMatchSupportSQL("identity_match_evidence_sources.source_id")+`
 AND NOT `+emailEvidencePairForSourceSQL("identity_match_evidence_sources.source_id")+`)`); err != nil {
		return fmt.Errorf("prune unsupported independent-evidence corroboration: %w", err)
	}
	return nil
}
