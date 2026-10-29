package store

import (
	"context"
	"errors"
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

// dropUnsupportedGeneratedNameEvidence removes generated display-name
// explanations whose participants no longer both carry a Full-supported name
// equal to the evidence detail. Replacing or tombstoning the envelope that
// carried a name withdraws that support without rewriting the evidence row.
// Names compare with strings.EqualFold, as the matcher does when it records
// them; SQL case folding is ASCII-only on SQLite. Manual evidence is kept.
func dropUnsupportedGeneratedNameEvidence(
	ctx context.Context, q identityMatchReviewQuerier, evidence []IdentityMatchEvidence,
) ([]IdentityMatchEvidence, error) {
	var ids []int64
	for _, item := range evidence {
		if item.EvidenceKind == "display_name" && item.Source == ProvenanceArchiveObservation {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return evidence, nil
	}
	unsupported := make(map[int64]bool, len(ids))
	supported := func(side string) string {
		return `CASE WHEN ` + FullParticipantNameWhere(side+".id", side+".display_name") + ` THEN 1 ELSE 0 END`
	}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := q.QueryContext(ctx, `SELECT e.id, COALESCE(e.detail,''),
			COALESCE(l.display_name,''), `+supported("l")+`, COALESCE(r.display_name,''), `+supported("r")+`
			FROM identity_match_evidence e
			JOIN identity_match_candidates c ON c.id = e.candidate_id
			LEFT JOIN participants l ON l.id = c.left_id AND c.left_kind = 'participant'
			LEFT JOIN participants r ON r.id = c.right_id AND c.right_kind = 'participant'
			WHERE e.id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("check generated name evidence support: %w", err)
		}
		for rows.Next() {
			var id int64
			var detail, left, right string
			var leftSupported, rightSupported int
			if err := rows.Scan(&id, &detail, &left, &leftSupported, &right, &rightSupported); err != nil {
				_ = rows.Close()
				return nil, err
			}
			ok := detail != "" && leftSupported == 1 && rightSupported == 1 &&
				strings.EqualFold(left, detail) && strings.EqualFold(right, detail)
			unsupported[id] = !ok
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	kept := evidence[:0:0]
	for _, item := range evidence {
		if !unsupported[item.ID] {
			kept = append(kept, item)
		}
	}
	return kept, nil
}
