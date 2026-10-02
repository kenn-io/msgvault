package personenrollment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// PolicyUpdate contains only existing non-secret policy fields. Nil preserves
// the current value; an empty string or false is an explicit replacement.
type PolicyUpdate struct {
	Model            *string   `json:"model,omitempty"`
	RetentionPosture *string   `json:"retention_posture,omitempty"`
	TrainingPosture  *string   `json:"training_posture,omitempty"`
	AllowedSources   *[]string `json:"allowed_sources,omitempty"`
	SourceSince      *string   `json:"source_since,omitempty"`
	SourceUntil      *string   `json:"source_until,omitempty"`
	AllowSensitive   *bool     `json:"allow_sensitive,omitempty"`
	ReasoningEffort  *string   `json:"reasoning_effort,omitempty"`
	ReasoningMode    *string   `json:"reasoning_mode,omitempty"`
	RequestTimeout   *string   `json:"request_timeout,omitempty"`
}

func (u PolicyUpdate) HasChanges() bool {
	return u.Model != nil || u.RetentionPosture != nil || u.TrainingPosture != nil || u.AllowedSources != nil || u.SourceSince != nil || u.SourceUntil != nil || u.AllowSensitive != nil || u.ReasoningEffort != nil || u.ReasoningMode != nil || u.RequestTimeout != nil
}

func (u PolicyUpdate) Apply(provider peoplesweep.ProviderConfig) (peoplesweep.ProviderConfig, error) {
	for _, field := range []struct {
		input  *string
		target *string
	}{{u.Model, &provider.Model}, {u.RetentionPosture, &provider.RetentionPosture}, {u.TrainingPosture, &provider.TrainingPosture}, {u.SourceSince, &provider.SourceSince}, {u.SourceUntil, &provider.SourceUntil}, {u.ReasoningEffort, &provider.ReasoningEffort}, {u.ReasoningMode, &provider.ReasoningMode}} {
		if field.input != nil {
			*field.target = *field.input
		}
	}
	if u.AllowedSources != nil {
		provider.AllowedSources = make([]peoplesweep.SourceClass, len(*u.AllowedSources))
		for i, source := range *u.AllowedSources {
			provider.AllowedSources[i] = peoplesweep.SourceClass(source)
		}
	}
	if u.AllowSensitive != nil {
		provider.AllowSensitive = *u.AllowSensitive
	}
	if u.RequestTimeout != nil {
		duration, err := time.ParseDuration(*u.RequestTimeout)
		if err != nil {
			return peoplesweep.ProviderConfig{}, fmt.Errorf("parse people provider request timeout: %w", err)
		}
		provider.RequestTimeout = duration
	}
	return provider, nil
}

// ProviderUpdateEdit is the existing host setter's credential-preserving
// table edit. Explicit empty optional values must clear their old entries.
func ProviderUpdateEdit(name string, provider peoplesweep.ProviderConfig) config.TableEdit {
	values := peoplesweep.ProviderTOMLValues(provider)
	values["source_until"] = provider.SourceUntil
	values["reasoning_effort"] = provider.ReasoningEffort
	values["reasoning_mode"] = provider.ReasoningMode
	if provider.Protocol == peoplesweep.ProtocolOpenAIChat {
		values["token_limit_parameter"] = provider.TokenLimitParameter
	}
	return config.TableEdit{Path: []string{"people", "sweep", "providers", name}, Values: values}
}

// PolicyUpdateHooks bind the same transaction to host CLI or daemon ownership.
// Negotiation and validation happen before it starts. Check runs only after
// publication and a second revocation; failure restores the exact saved file.
type PolicyUpdateHooks struct {
	Revoke  func(context.Context, bool) error
	Publish func(string) (config.ConfigFile, error)
	Check   func(context.Context, config.ConfigFile) error
	Restore func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error)
}

// PolicyUpdateError retains rollback facts without exposing diagnostics.
type PolicyUpdateError struct {
	Phase      string
	RolledBack bool
	cause      error
}

func (e *PolicyUpdateError) Error() string { return e.cause.Error() }
func (e *PolicyUpdateError) Unwrap() error { return e.cause }

func UpdatePolicy(ctx context.Context, before config.ConfigFile, hooks PolicyUpdateHooks) (config.ConfigFile, error) {
	if hooks.Revoke == nil || hooks.Publish == nil || hooks.Check == nil || hooks.Restore == nil {
		return config.ConfigFile{}, errors.New("people provider policy transaction is unavailable")
	}
	if err := hooks.Revoke(ctx, true); err != nil {
		return config.ConfigFile{}, err
	}
	after, err := hooks.Publish(before.ETag)
	rollback := func(phase string, cause error) (config.ConfigFile, error) {
		var restoreErr error
		if !after.Exists || after.ETag == "" {
			restoreErr = errors.New("cannot roll back people provider config without a verified published snapshot")
		} else {
			_, restoreErr = hooks.Restore(after, before)
			if restoreErr != nil {
				restoreErr = fmt.Errorf("restore people provider config: %w", restoreErr)
			}
		}
		return after, &PolicyUpdateError{Phase: phase, RolledBack: restoreErr == nil, cause: errors.Join(cause, restoreErr, errors.New("exact people provider consent remains revoked"))}
	}
	if err != nil {
		return rollback("publish", err)
	}
	if err := hooks.Revoke(ctx, false); err != nil {
		return rollback("revoke", err)
	}
	if err := hooks.Check(ctx, after); err != nil {
		return rollback("check", err)
	}
	return after, nil
}
