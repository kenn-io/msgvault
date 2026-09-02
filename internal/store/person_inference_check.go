package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// PersonInferenceCheck records one successful synthetic capability check for
// an exact immutable provider profile. It contains no credential values.
type PersonInferenceCheck struct {
	ProfileFingerprint string                 `json:"profile_fingerprint"`
	CheckedAt          time.Time              `json:"checked_at"`
	DriverVersion      string                 `json:"driver_version"`
	OutputMode         peoplesweep.OutputMode `json:"output_mode"`
	ProviderRequestID  string                 `json:"provider_request_id,omitempty"`
	ModelVersion       string                 `json:"model_version"`
}

// RecordPersonInferenceCheck records or replaces safe check metadata for one
// existing exact profile. It never creates a profile or consent grant.
func (s *Store) RecordPersonInferenceCheck(
	ctx context.Context,
	check PersonInferenceCheck,
) error {
	if err := validatePersonInferenceCheck(check); err != nil {
		return err
	}
	var driverVersion, outputMode string
	err := s.db.QueryRowContext(ctx, `
		SELECT driver_version, output_mode
		FROM person_inference_profiles
		WHERE fingerprint = ?`, check.ProfileFingerprint).Scan(&driverVersion, &outputMode)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("people inference check profile does not exist")
	}
	if err != nil {
		return fmt.Errorf("read people inference check profile: %w", err)
	}
	if check.DriverVersion != driverVersion || string(check.OutputMode) != outputMode {
		return errors.New("people inference check does not match the immutable provider profile")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO person_inference_checks
			(profile_fingerprint, checked_at, driver_version, output_mode,
			 provider_request_id, model_version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (profile_fingerprint) DO UPDATE SET
			checked_at = excluded.checked_at,
			driver_version = excluded.driver_version,
			output_mode = excluded.output_mode,
			provider_request_id = excluded.provider_request_id,
			model_version = excluded.model_version`,
		check.ProfileFingerprint, check.CheckedAt.UTC(), check.DriverVersion,
		check.OutputMode, check.ProviderRequestID, check.ModelVersion,
	)
	if err != nil {
		return fmt.Errorf("record people inference check: %w", err)
	}
	return nil
}

// GetPersonInferenceCheck returns the successful check for one exact profile.
// A missing check is reported as (nil, nil).
func (s *Store) GetPersonInferenceCheck(
	ctx context.Context,
	fingerprint string,
) (*PersonInferenceCheck, error) {
	if !validLowerSHA256(fingerprint) {
		return nil, errors.New("people inference check requires a lowercase SHA-256 fingerprint")
	}
	var check PersonInferenceCheck
	var checkedAt nullableTimestamp
	var outputMode, profileDriverVersion, profileOutputMode string
	err := s.db.QueryRowContext(ctx, `
		SELECT check_row.profile_fingerprint, check_row.checked_at,
		       check_row.driver_version, check_row.output_mode,
		       check_row.provider_request_id, check_row.model_version,
		       profile.driver_version, profile.output_mode
		FROM person_inference_checks check_row
		JOIN person_inference_profiles profile
		  ON profile.fingerprint = check_row.profile_fingerprint
		WHERE check_row.profile_fingerprint = ?`, fingerprint).Scan(
		&check.ProfileFingerprint, &checkedAt, &check.DriverVersion, &outputMode,
		&check.ProviderRequestID, &check.ModelVersion, &profileDriverVersion, &profileOutputMode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // A missing check is the documented optional result.
	}
	if err != nil {
		return nil, fmt.Errorf("read people inference check: %w", err)
	}
	if !checkedAt.Valid {
		return nil, errors.New("people inference check has invalid checked_at")
	}
	check.CheckedAt = checkedAt.Time.UTC()
	check.OutputMode = peoplesweep.OutputMode(outputMode)
	if err := validatePersonInferenceCheck(check); err != nil {
		return nil, fmt.Errorf("stored people inference check is invalid: %w", err)
	}
	if check.DriverVersion != profileDriverVersion || outputMode != profileOutputMode {
		return nil, errors.New("stored people inference check does not match the immutable provider profile")
	}
	return &check, nil
}

// HasSuccessfulPersonInferenceCheck implements the runner's exact profile
// verification gate.
func (s *Store) HasSuccessfulPersonInferenceCheck(
	ctx context.Context,
	fingerprint string,
) (bool, error) {
	check, err := s.GetPersonInferenceCheck(ctx, fingerprint)
	if err != nil {
		return false, err
	}
	return check != nil, nil
}

func validatePersonInferenceCheck(check PersonInferenceCheck) error {
	if !validLowerSHA256(check.ProfileFingerprint) {
		return errors.New("people inference check requires a lowercase SHA-256 fingerprint")
	}
	if check.CheckedAt.IsZero() {
		return errors.New("people inference check checked_at is required")
	}
	if check.DriverVersion == "" || !peoplesweep.IsSafeProviderMetadata(check.DriverVersion) {
		return errors.New("people inference check driver_version is invalid")
	}
	switch check.OutputMode {
	case peoplesweep.OutputModeNativeJSONSchema,
		peoplesweep.OutputModeJSONObject,
		peoplesweep.OutputModePromptJSON:
	default:
		return errors.New("people inference check output_mode is invalid")
	}
	if !peoplesweep.IsSafeProviderMetadata(check.ProviderRequestID) {
		return errors.New("people inference check provider_request_id is invalid")
	}
	if check.ModelVersion == "" || !peoplesweep.IsSafeProviderMetadata(check.ModelVersion) {
		return errors.New("people inference check model_version is invalid")
	}
	return nil
}
