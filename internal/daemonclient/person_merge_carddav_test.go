package daemonclient

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafeMCPErrorPreservesIdentityReviewCodes(t *testing.T) {
	for _, code := range []string{
		"identity_match_state_unavailable", "identity_match_not_found", "identity_match_review_stale",
		"identity_match_not_acceptable", "identity_match_already_accepted", "identity_match_already_applied",
		"identity_match_state_changed", "identity_match_endpoint_unsupported", "identity_match_failed",
		"person_binding_conflict",
	} {
		t.Run(code, func(t *testing.T) {
			err := SafeMCPError(&APIError{Status: 409, Code: code, Message: "Synthetic daemon failure detail"})
			require.EqualError(t, err, fmt.Sprintf("daemon request failed (409, %s)", code))
		})
	}
}

func TestSafeMCPErrorIncludesInvalidLimitCode(t *testing.T) {
	err := SafeMCPError(&APIError{
		Status:  400,
		Code:    "invalid_limit",
		Message: "Limit exceeds configured batch size",
	})

	require.Error(t, err)
	require.EqualError(t, err, "daemon request failed (400, invalid_limit)")
}

func TestSafeMCPErrorPreservesServedRouteCodesWithoutProse(t *testing.T) {
	for _, code := range []string{
		"invalid_if_match", "invalid_idempotency_key", "if_match_required", "idempotency_key_required",
		"person_profile_not_found", "person_merge_invalid", "person_merge_failed",
		"carddav_unavailable", "google_authorization_required", "microsoft_authorization_required", "carddav_preview_too_large",
		"carddav_conflict_stale", "carddav_conflict_pending", "carddav_publication_pending",
		"carddav_retry_after", "carddav_upstream_failed", "carddav_storage_failed", "carddav_failed",
		"bad_request", "not_found", "conflict", "invalid_request",
		"invalid_config", "feature_unavailable", "scoring_status_unavailable", "history_unavailable",
		"operation_in_progress", "server_busy",
		"identity_matches_unavailable", "invalid_identity_match_state", "review_token_required",
		"invalid_candidate_id", "invalid_participant_id", "invalid_before_id",
	} {
		t.Run(code, func(t *testing.T) {
			err := SafeMCPError(&APIError{Status: 400, Code: code, Message: "Synthetic private failure detail"})
			require.EqualError(t, err, fmt.Sprintf("daemon request failed (400, %s)", code))
		})
	}
}

func TestSafeMCPErrorGivesFixedGuidanceForAContactTooLargeForOutlook(t *testing.T) {
	err := SafeMCPError(&APIError{Status: 413, Code: "microsoft_contact_too_large", Message: "Synthetic private failure detail"})
	require.EqualError(t, err, "daemon request failed (413, microsoft_contact_too_large): A published contact is over Outlook's 4 MB limit, and each sync reports it until it fits. The daemon log names its person ID. Removing stored photos or other media currently needs the profile API; the CLI and Web UI have no control for it.")
}

func TestSafeMCPErrorHidesUnknownDaemonCodeAndMessage(t *testing.T) {
	err := SafeMCPError(&APIError{
		Status:  400,
		Code:    "private_detail",
		Message: "private contact data",
	})

	require.Error(t, err)
	require.EqualError(t, err, "daemon request failed (400)")
}
