package personenrichment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/personfacts"
)

type WorkerOptions struct {
	Owner           string
	LeaseDuration   time.Duration
	RenewEvery      time.Duration
	Clock           func() time.Time
	Jitter          func(time.Duration) time.Duration
	ProviderConfigs map[string]ProviderConfig
}

type Worker struct {
	work          WorkStore
	sink          ClaimSink
	gate          EgressGate
	providers     map[string]ProviderFactory
	providerNames []string
	options       WorkerOptions
}

type leaseTokenState struct {
	mu          sync.Mutex
	current     LeaseToken
	generation  uint64
	bindingDone chan struct{}
}

func newLeaseTokenState(token LeaseToken) *leaseTokenState {
	return &leaseTokenState{current: token}
}

func (s *leaseTokenState) Renew(
	renew func(LeaseToken) error, cancelCurrent func(),
) error {
	for {
		s.mu.Lock()
		if s.bindingDone != nil {
			done := s.bindingDone
			s.mu.Unlock()
			<-done
			continue
		}
		token := s.current
		generation := s.generation
		s.mu.Unlock()

		err := renew(token)

		s.mu.Lock()
		if s.bindingDone != nil {
			done := s.bindingDone
			s.mu.Unlock()
			<-done
			continue
		}
		if s.generation != generation {
			s.mu.Unlock()
			continue
		}
		if err != nil {
			cancelCurrent()
		}
		s.mu.Unlock()
		return err
	}
}

func (s *leaseTokenState) Begin(
	begin func(LeaseToken) (*DurableAttempt, bool, error),
) (*DurableAttempt, bool, error) {
	s.mu.Lock()
	token := s.current
	done := make(chan struct{})
	s.bindingDone = done
	s.mu.Unlock()

	attempt, created, err := begin(token)

	s.mu.Lock()
	if err == nil && attempt != nil {
		s.current = attempt.Token
		s.generation++
	}
	s.bindingDone = nil
	close(done)
	s.mu.Unlock()
	return attempt, created, err
}

func NewWorker(
	work WorkStore,
	sink ClaimSink,
	gate EgressGate,
	providers map[string]ProviderFactory,
	options WorkerOptions,
) (*Worker, error) {
	options.Owner = strings.TrimSpace(options.Owner)
	if work == nil || sink == nil {
		return nil, errors.New("person enrichment worker requires work store and claim sink")
	}
	if err := gate.validate(); err != nil {
		return nil, fmt.Errorf("validate person enrichment egress gate: %w", err)
	}
	if options.Owner == "" || options.LeaseDuration <= 0 || options.RenewEvery <= 0 ||
		options.RenewEvery > options.LeaseDuration/3 || options.Clock == nil || options.Jitter == nil {
		return nil, errors.New("person enrichment worker options are invalid")
	}
	if len(providers) == 0 {
		return nil, errors.New("person enrichment worker requires at least one provider factory")
	}
	factoryCopy := make(map[string]ProviderFactory, len(providers))
	names := make([]string, 0, len(providers))
	for rawName, factory := range providers {
		name := strings.TrimSpace(rawName)
		if name == "" || name != rawName || factory == nil {
			return nil, errors.New("person enrichment provider factory map is invalid")
		}
		factoryCopy[name] = factory
		names = append(names, name)
	}
	sort.Strings(names)
	configCopy := make(map[string]ProviderConfig, len(options.ProviderConfigs))
	for name, config := range options.ProviderConfigs {
		configCopy[name] = cloneWorkerProviderConfig(config)
	}
	options.ProviderConfigs = configCopy
	return &Worker{
		work: work, sink: sink, gate: gate, providers: factoryCopy,
		providerNames: names, options: options,
	}, nil
}

func cloneWorkerProviderConfig(config ProviderConfig) ProviderConfig {
	cloned := config
	cloned.AllowedIdentifiers = slices.Clone(config.AllowedIdentifiers)
	cloned.TargetKeys = slices.Clone(config.TargetKeys)
	return cloned
}

// RunOnce claims and advances at most one durable item. Provider outcomes are
// persisted and returned as processed work; infrastructure and fencing errors
// remain visible to the scheduler.
func (w *Worker) RunOnce(ctx context.Context, runID int64) (processed bool, err error) {
	if w == nil || runID <= 0 {
		return false, errors.New("person enrichment worker requires a positive durable run ID")
	}
	for _, providerName := range w.providerNames {
		now := w.options.Clock().UTC()
		if now.IsZero() {
			return false, errors.New("person enrichment worker clock returned zero time")
		}
		lease, claimErr := w.work.ClaimWork(ctx, ClaimOptions{
			RunID: runID, Owner: w.options.Owner, ProviderName: providerName,
			Now: now, LeaseDuration: w.options.LeaseDuration,
		})
		if claimErr != nil {
			return false, claimErr
		}
		if lease == nil {
			continue
		}
		return true, w.processWithLeaseRenewal(ctx, *lease)
	}
	return false, nil
}

func (w *Worker) processWithLeaseRenewal(ctx context.Context, lease WorkLease) error {
	leaseContext, cancel := context.WithCancel(ctx)
	defer cancel()
	tokens := newLeaseTokenState(lease.Token)
	renewDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(w.options.RenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-leaseContext.Done():
				renewDone <- nil
				return
			case <-ticker.C:
				until := w.options.Clock().UTC().Add(w.options.LeaseDuration)
				renewalCanceled := false
				if err := tokens.Renew(func(token LeaseToken) error {
					return w.work.RenewLease(leaseContext, token, until)
				}, func() {
					if leaseContext.Err() != nil {
						return
					}
					renewalCanceled = true
					cancel()
				}); err != nil {
					if leaseContext.Err() != nil && !renewalCanceled {
						renewDone <- nil
						return
					}
					renewDone <- fmt.Errorf("renew person enrichment lease: %w", err)
					return
				}
			}
		}
	}()

	processErr := w.processLease(leaseContext, lease, tokens)
	cancel()
	renewErr := <-renewDone
	if renewErr != nil {
		return renewErr
	}
	return processErr
}

func (w *Worker) processLease(
	ctx context.Context, lease WorkLease, tokens *leaseTokenState,
) error {
	profile, err := w.work.LoadProviderProfile(ctx, lease.ProfileFingerprint)
	if err != nil {
		return w.releaseRetry(ctx, lease.Token, nil, safeFailure(FailureTransient, 0, "", "profile unavailable"), 0)
	}
	if profile.Name != lease.ProviderName || profile.Fingerprint != lease.ProfileFingerprint {
		return w.releaseRetry(ctx, lease.Token, nil, safeFailure(FailurePolicy, 0, "", "profile binding changed"), 0)
	}
	input, err := w.work.LoadRequestInput(ctx, lease)
	if err != nil {
		return w.releaseRetry(ctx, lease.Token, nil, safeFailure(FailureTransient, 0, "", "request state unavailable"), 0)
	}
	request, hashes, err := BuildRequest(input, profile)
	if err != nil {
		return w.terminalizePolicyDrift(ctx, lease, input, RequestHashes{})
	}

	config, ok := w.options.ProviderConfigs[lease.ProviderName]
	if !ok {
		return w.terminalizePolicyDrift(ctx, lease, input, hashes)
	}
	configuredProfile, err := config.Profile(personfacts.Catalog{Targets: profile.Targets})
	if err != nil || !reflect.DeepEqual(configuredProfile, profile) {
		return w.terminalizePolicyDrift(ctx, lease, input, hashes)
	}
	if config.Name != lease.ProviderName || !config.Enabled {
		return w.terminalizePolicyDrift(ctx, lease, input, hashes)
	}
	if lease.ActiveAttempt != nil {
		if err := verifyDurableRequestBinding(lease, input, request, hashes); err != nil {
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailureInvalidOutput, 0, "", "durable request binding changed"))
		}
	}

	knownIDs, err := w.work.LoadProviderPersonIDs(ctx, lease.PersonID, profile.ProviderNamespace)
	if err != nil {
		return w.releaseRetry(ctx, lease.Token, &input,
			safeFailure(FailureTransient, 0, "", "provider identity state unavailable"), 0)
	}
	authorization, err := w.gate.Authorize(ctx, EgressInput{
		Request: request, Profile: profile, KnownProviderPersonIDs: knownIDs,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrSuppressed):
			if lease.ActiveAttempt == nil {
				return w.releaseTerminalBeforeAttempt(ctx, lease, input, hashes, "suppressed")
			}
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailureSuppressed, 0, "", "egress suppressed"))
		case errors.Is(err, ErrCredentialUnavailable):
			return w.releaseRetry(ctx, lease.Token, &input,
				safeFailure(FailureTransient, 0, "", "credential unavailable"), activeAttemptCount(lease))
		case errors.Is(err, ErrConsentRequired), errors.Is(err, ErrSuppressionKeyMismatch):
			if lease.ActiveAttempt == nil {
				return w.releaseTerminalBeforeAttempt(ctx, lease, input, hashes, "policy")
			}
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailurePolicy, 0, "", "egress policy rejected"))
		default:
			return w.releaseRetry(ctx, lease.Token, &input,
				safeFailure(FailureTransient, 0, "", "egress policy state unavailable"), activeAttemptCount(lease))
		}
	}

	factory := w.providers[lease.ProviderName]
	provider, err := factory(config, authorization.Credential)
	authorization.Credential = ""
	if err != nil || provider == nil {
		return w.releaseRetry(ctx, lease.Token, &input,
			safeFailure(FailureTransient, 0, "", "provider construction failed"), activeAttemptCount(lease))
	}
	if lease.ActiveAttempt != nil {
		return w.resumeAttempt(ctx, lease, input, request, hashes, profile, config, provider,
			authorization, knownIDs, tokens)
	}
	return w.beginAndStart(ctx, lease, input, request, hashes, profile, config, provider,
		authorization, knownIDs, tokens)
}

func verifyDurableRequestBinding(
	lease WorkLease, input RequestInput, request Request, hashes RequestHashes,
) error {
	active := lease.ActiveAttempt
	if active == nil || active.ID != lease.Token.AttemptID || active.RunID != lease.RunID ||
		active.Token.WorkPersonID != lease.PersonID || active.Token.ProfileFingerprint != lease.ProfileFingerprint ||
		active.RequestHash != request.RequestHash || active.RequestHash != hashes.RequestHash ||
		active.PayloadHash != hashes.PayloadHash || active.PersonRevision != input.PersonRevision ||
		active.Trigger != lease.Trigger {
		return errors.New("durable enrichment attempt does not match current request")
	}
	return nil
}

func (w *Worker) beginAndStart(
	ctx context.Context,
	lease WorkLease,
	input RequestInput,
	request Request,
	hashes RequestHashes,
	profile ProviderProfile,
	config ProviderConfig,
	provider Provider,
	authorization Authorization,
	knownIDs []string,
	tokens *leaseTokenState,
) error {
	guaranteed, ok := guaranteedCharge(ctx, provider, request, profileHasHardCostCap(profile))
	if !ok {
		return w.releaseTerminalBeforeAttempt(ctx, lease, input, hashes, "policy")
	}
	attempt, _, err := tokens.Begin(func(token LeaseToken) (*DurableAttempt, bool, error) {
		return w.work.BeginAttempt(ctx, token, AttemptStart{
			RunID: lease.RunID, PersonID: lease.PersonID, ProfileFingerprint: lease.ProfileFingerprint,
			PayloadHash: hashes.PayloadHash, RequestHash: hashes.RequestHash,
			PersonRevision: input.PersonRevision, Trigger: lease.Trigger,
			HardCostCap: profileHasHardCostCap(profile), GuaranteedMaxCost: guaranteed,
			CheckedIdentifiers: authorization.CheckedIdentifiers,
		})
	})
	if err != nil {
		if errors.Is(err, ErrRequestBudgetExceeded) || errors.Is(err, ErrCostBudgetExceeded) {
			return w.deferBudgetExhausted(ctx, lease.Token, input)
		}
		if errors.Is(err, ErrSuppressed) {
			return w.releaseTerminalBeforeAttempt(ctx, lease, input, hashes, "suppressed")
		}
		if errors.Is(err, ErrAccountingDisabled) {
			return w.releaseTerminalBeforeAttempt(ctx, lease, input, hashes, "policy")
		}
		return fmt.Errorf("begin person enrichment attempt: %w", err)
	}
	lease.Token = attempt.Token
	lease.ActiveAttempt = attempt
	return w.startAttempt(ctx, lease, request, profile, config, provider, knownIDs)
}

func (w *Worker) deferBudgetExhausted(
	ctx context.Context, token LeaseToken, input RequestInput,
) error {
	now := w.options.Clock().UTC()
	nextDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	failure := safeFailure(FailurePolicy, 0, "", "enrichment budget exhausted")
	return w.work.ReleaseWork(ctx, token, WorkRelease{
		Outcome: "defer", Failure: &failure, NextActionAt: &nextDay,
		PersonRevision: input.PersonRevision,
	})
}

func guaranteedCharge(
	ctx context.Context, provider Provider, request Request, required bool,
) (Cost, bool) {
	if !required {
		return Cost{}, true
	}
	bounded, ok := provider.(GuaranteedChargeProvider)
	if !ok {
		return Cost{}, false
	}
	cost, err := bounded.GuaranteedMaxCharge(ctx, request)
	if err != nil || cost.ValidateGuaranteed() != nil {
		return Cost{}, false
	}
	return cost, true
}

func profileHasHardCostCap(profile ProviderProfile) bool {
	return profile.MaxCostUSDMicrosPerPersonPerDay > 0 || profile.MaxCostUSDMicrosPerRun > 0 ||
		profile.MaxCostUSDMicrosPerDay > 0
}

func (w *Worker) resumeAttempt(
	ctx context.Context,
	lease WorkLease,
	input RequestInput,
	request Request,
	hashes RequestHashes,
	profile ProviderProfile,
	config ProviderConfig,
	provider Provider,
	authorization Authorization,
	knownIDs []string,
	tokens *leaseTokenState,
) error {
	active := lease.ActiveAttempt
	switch active.State {
	case "retry_wait":
		if active.JobID != "" {
			return w.pollAttempt(ctx, lease, request, profile, config, provider, knownIDs)
		}
		if active.RequestID != "" {
			return w.work.MarkUncertainStart(ctx, lease.Token,
				safeFailure(FailureUncertainStart, 0, active.RequestID, "jobless start cannot be replayed"))
		}
		if active.AttemptCount > int64(config.MaxRetries)+1 {
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailureTerminal, 0, "", "retry limit reached"))
		}
		attempt, _, err := tokens.Begin(func(token LeaseToken) (*DurableAttempt, bool, error) {
			return w.work.BeginAttempt(ctx, token, AttemptStart{
				RunID: lease.RunID, PersonID: lease.PersonID, ProfileFingerprint: lease.ProfileFingerprint,
				PayloadHash: hashes.PayloadHash, RequestHash: hashes.RequestHash,
				PersonRevision: input.PersonRevision, Trigger: lease.Trigger,
				HardCostCap:        profileHasHardCostCap(profile),
				GuaranteedMaxCost:  durableGuaranteedCost(active),
				CheckedIdentifiers: authorization.CheckedIdentifiers,
			})
		})
		if err != nil {
			return err
		}
		lease.Token = attempt.Token
		lease.ActiveAttempt = attempt
		return w.startAttempt(ctx, lease, request, profile, config, provider, knownIDs)
	case "pending":
		if active.JobID == "" {
			return w.work.MarkUncertainStart(ctx, lease.Token,
				safeFailure(FailureUncertainStart, 0, active.RequestID, "jobless start cannot be replayed"))
		}
		return w.pollAttempt(ctx, lease, request, profile, config, provider, knownIDs)
	case "uncertain_start", "starting":
		return w.work.MarkUncertainStart(ctx, lease.Token,
			safeFailure(FailureUncertainStart, 0, active.RequestID, "start outcome is uncertain"))
	default:
		return w.work.MarkTerminal(ctx, lease.Token,
			safeFailure(FailureInvalidOutput, 0, active.RequestID, "invalid durable attempt state"))
	}
}

func durableGuaranteedCost(active *DurableAttempt) Cost {
	if active == nil || !active.HardCostCap {
		return Cost{}
	}
	return Cost{Currency: "USD", AmountMicros: active.ReservedCostMicros}
}

func (w *Worker) startAttempt(
	ctx context.Context,
	lease WorkLease,
	request Request,
	profile ProviderProfile,
	config ProviderConfig,
	provider Provider,
	knownIDs []string,
) error {
	if err := w.work.AuthorizeAttemptDispatch(ctx, lease.Token); err != nil {
		if errors.Is(err, ErrSuppressed) {
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailureSuppressed, 0, "", "enrichment suppressed before dispatch"))
		}
		if errors.Is(err, ErrConsentRequired) {
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailurePolicy, 0, "", "enrichment consent revoked before dispatch"))
		}
		return fmt.Errorf("authorize person enrichment dispatch: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, config.RequestTimeout)
	started, err := provider.Start(callCtx, request)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return w.persistProviderFailure(ctx, lease, config, err, true)
	}
	if err := validateStartedAttempt(started, request); err != nil {
		return w.work.MarkTerminal(ctx, lease.Token,
			safeFailure(FailureInvalidOutput, 0, started.RequestID, "invalid provider start"))
	}
	if started.State == AttemptPending {
		if err := w.work.RecordProviderStarted(ctx, lease.Token, started); err != nil {
			return err
		}
		lease.ActiveAttempt = durableAttemptFromStart(lease, started)
		return w.work.SchedulePoll(ctx, lease.Token, Result{
			State: ResultPending, RequestID: started.RequestID, JobID: started.JobID,
			PollAfter: started.PollAfter, AdapterVersion: started.AdapterVersion,
			SchemaVersion: started.SchemaVersion, GeneratedSchema: started.GeneratedSchema,
			GeneratedSchemaHash: started.GeneratedSchemaHash,
		})
	}
	return w.completeAttempt(ctx, lease, request, profile, knownIDs, *started.Result)
}

func validateStartedAttempt(started Attempt, request Request) error {
	if err := started.Validate(); err != nil {
		return err
	}
	fingerprint, err := ProgramFingerprint(ProgramDescriptor{
		HostMappingVersion: HostClaimMappingVersion, AdapterVersion: started.AdapterVersion,
		WireSchemaVersion: started.SchemaVersion, GeneratedSchema: started.GeneratedSchema,
		GeneratedSchemaHash: started.GeneratedSchemaHash,
	})
	if err != nil || fingerprint != started.ProgramFingerprint {
		return errors.New("provider start program fingerprint mismatch")
	}
	if started.State == AttemptPending {
		if started.PollAfter <= 0 || started.StartedAt.IsZero() {
			return errors.New("pending provider start requires timing metadata")
		}
		if started.GeneratedSchema {
			_, durable, err := EncodeDurableAttemptTargets(started.Targets)
			if err != nil || !reflect.DeepEqual(durable, request.Targets) {
				return errors.New("pending provider targets changed")
			}
		} else if len(started.Targets) != 0 {
			return errors.New("fixed provider start has durable targets")
		}
	}
	if started.State == AttemptComplete {
		return validateResultEnvelope(*started.Result, started.RequestID, started.JobID,
			started.AdapterVersion, started.SchemaVersion, started.GeneratedSchema,
			started.GeneratedSchemaHash, started.ProgramFingerprint)
	}
	return nil
}

func durableAttemptFromStart(lease WorkLease, started Attempt) *DurableAttempt {
	active := lease.ActiveAttempt
	if active == nil {
		active = &DurableAttempt{ID: lease.Token.AttemptID, RunID: lease.RunID, Token: lease.Token}
	}
	active.State = "pending"
	active.RequestID = started.RequestID
	active.JobID = started.JobID
	active.AdapterVersion = started.AdapterVersion
	active.SchemaVersion = started.SchemaVersion
	active.GeneratedSchema = started.GeneratedSchema
	active.GeneratedSchemaHash = started.GeneratedSchemaHash
	active.ProgramFingerprint = started.ProgramFingerprint
	active.Targets = cloneDurableAttemptTargets(started.Targets)
	active.StartedAt = started.StartedAt
	return active
}

func (w *Worker) pollAttempt(
	ctx context.Context,
	lease WorkLease,
	request Request,
	profile ProviderProfile,
	config ProviderConfig,
	provider Provider,
	knownIDs []string,
) error {
	active := lease.ActiveAttempt
	if err := validateDurablePollAttempt(active); err != nil {
		return w.work.MarkTerminal(ctx, lease.Token,
			safeFailure(FailureInvalidOutput, 0, active.RequestID, "invalid durable poll binding"))
	}
	pollInput := Attempt{
		State: AttemptPending, RequestID: active.RequestID, JobID: active.JobID,
		StartedAt: active.StartedAt, AdapterVersion: active.AdapterVersion,
		SchemaVersion: active.SchemaVersion, GeneratedSchema: active.GeneratedSchema,
		GeneratedSchemaHash: active.GeneratedSchemaHash,
		ProgramFingerprint:  active.ProgramFingerprint, Targets: cloneDurableAttemptTargets(active.Targets),
	}
	callCtx, cancel := context.WithTimeout(ctx, config.RequestTimeout)
	result, err := provider.Poll(callCtx, pollInput)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return w.persistProviderFailure(ctx, lease, config, err, false)
	}
	if err := validateResultEnvelope(result, active.RequestID, active.JobID,
		active.AdapterVersion, active.SchemaVersion, active.GeneratedSchema,
		active.GeneratedSchemaHash, active.ProgramFingerprint); err != nil {
		return w.work.MarkTerminal(ctx, lease.Token,
			safeFailure(FailureInvalidOutput, 0, active.RequestID, "invalid provider poll result"))
	}
	if result.State == ResultPending {
		if result.PollAfter <= 0 {
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailureInvalidOutput, 0, active.RequestID, "invalid provider poll delay"))
		}
		return w.work.SchedulePoll(ctx, lease.Token, result)
	}
	return w.completeAttempt(ctx, lease, request, profile, knownIDs, result)
}

func validateDurablePollAttempt(active *DurableAttempt) error {
	if active == nil || active.ID <= 0 || active.RunID <= 0 || active.JobID == "" ||
		strings.TrimSpace(active.JobID) == "" || active.StartedAt.IsZero() {
		return errors.New("durable poll attempt is incomplete")
	}
	fingerprint, err := ProgramFingerprint(ProgramDescriptor{
		HostMappingVersion: HostClaimMappingVersion, AdapterVersion: active.AdapterVersion,
		WireSchemaVersion: active.SchemaVersion, GeneratedSchema: active.GeneratedSchema,
		GeneratedSchemaHash: active.GeneratedSchemaHash,
	})
	if err != nil || fingerprint != active.ProgramFingerprint {
		return errors.New("durable poll program fingerprint mismatch")
	}
	if active.GeneratedSchema {
		if _, _, err := EncodeDurableAttemptTargets(active.Targets); err != nil {
			return err
		}
	} else if len(active.Targets) != 0 {
		return errors.New("fixed poll attempt has durable targets")
	}
	return nil
}

func validateResultEnvelope(
	result Result,
	requestID, jobID, adapterVersion, schemaVersion string,
	generated bool,
	generatedHash, programFingerprint string,
) error {
	if err := result.Validate(); err != nil {
		return err
	}
	if result.RequestID != requestID || result.JobID != jobID ||
		result.AdapterVersion != adapterVersion || result.SchemaVersion != schemaVersion ||
		result.GeneratedSchema != generated || result.GeneratedSchemaHash != generatedHash {
		return errors.New("provider result tuple changed")
	}
	fingerprint, err := ProgramFingerprint(ProgramDescriptor{
		HostMappingVersion: HostClaimMappingVersion, AdapterVersion: result.AdapterVersion,
		WireSchemaVersion: result.SchemaVersion, GeneratedSchema: result.GeneratedSchema,
		GeneratedSchemaHash: result.GeneratedSchemaHash,
	})
	if err != nil || fingerprint != programFingerprint {
		return ErrProgramFingerprintMismatch
	}
	return nil
}

func (w *Worker) completeAttempt(
	ctx context.Context,
	lease WorkLease,
	request Request,
	profile ProviderProfile,
	knownIDs []string,
	result Result,
) error {
	verified := make([]ProviderPersonID, len(knownIDs))
	for i, id := range knownIDs {
		verified[i] = ProviderPersonID{ID: id}
	}
	assessment := AssessIdentity(request, result, verified)
	commit, err := NewClaimCommit(ClaimCommitInput{
		AttemptID: lease.Token.AttemptID, RunID: lease.RunID, PersonID: lease.PersonID,
		LeaseFence: lease.Token.Fence, ProfileFingerprint: lease.ProfileFingerprint,
		ProviderNamespace: profile.ProviderNamespace, RequestHash: request.RequestHash,
		IdentityAssessment: assessment,
	}, result, w.gate.Hasher)
	if err != nil {
		return w.work.MarkTerminal(ctx, lease.Token,
			safeFailure(FailureInvalidOutput, 0, result.RequestID, "invalid provider output"))
	}
	_, err = w.sink.CommitEnrichmentClaims(ctx, commit)
	return err
}

func (w *Worker) persistProviderFailure(
	ctx context.Context, lease WorkLease, config ProviderConfig, providerErr error, starting bool,
) error {
	failure, retryAfter := classifyProviderFailure(providerErr)
	if starting && failure.Class == FailureTransient {
		failure.Class = FailureUncertainStart
		failure.Message = "provider start outcome is uncertain"
	}
	if failure.Class == FailureUncertainStart {
		return w.work.MarkUncertainStart(ctx, lease.Token, failure)
	}
	if (failure.Class == FailureRateLimited || failure.Class == FailureTransient) &&
		activeAttemptCount(lease) <= int64(config.MaxRetries) {
		delay, err := RetryDelay(RetryInput{
			Attempt: int(activeAttemptCount(lease)), Base: time.Second,
			Maximum: httpretry.ProviderMaxRetryAfter, RetryAfter: retryAfter,
			Now: w.options.Clock().UTC(), Jitter: w.options.Jitter,
		})
		if err != nil {
			return w.work.MarkTerminal(ctx, lease.Token,
				safeFailure(FailureTerminal, failure.HTTPStatus, failure.ProviderRequestID, "invalid retry policy"))
		}
		if delay <= 0 {
			delay = time.Millisecond
		}
		return w.work.ScheduleRetry(ctx, lease.Token, RetryUpdate{
			Failure: failure, NextActionAt: w.options.Clock().UTC().Add(delay),
		})
	}
	return w.work.MarkTerminal(ctx, lease.Token, failure)
}

func classifyProviderFailure(err error) (SafeFailure, string) {
	if providerErr, ok := errors.AsType[*ProviderError](err); ok {
		class := providerErr.Class
		if class == "" {
			class = FailureTerminal
		}
		message := "provider request failed"
		if class == FailureUncertainStart {
			message = "provider start outcome is uncertain"
		}
		return safeFailure(class, providerErr.Status, providerErr.RequestID, message), providerErr.RetryAfter
	}
	return safeFailure(FailureUncertainStart, 0, "", "provider start outcome is uncertain"), ""
}

func (w *Worker) releaseTerminalBeforeAttempt(
	ctx context.Context,
	lease WorkLease,
	input RequestInput,
	hashes RequestHashes,
	outcome string,
) error {
	return w.work.ReleaseWork(ctx, lease.Token, WorkRelease{
		Outcome: outcome, PersonRevision: input.PersonRevision,
		PayloadHash: hashes.PayloadHash, RequestHash: hashes.RequestHash,
	})
}

func (w *Worker) terminalizePolicyDrift(
	ctx context.Context,
	lease WorkLease,
	input RequestInput,
	hashes RequestHashes,
) error {
	if lease.ActiveAttempt != nil {
		return w.work.MarkTerminal(ctx, lease.Token,
			safeFailure(FailurePolicy, 0, "", "enrichment policy changed"))
	}
	return w.releaseTerminalBeforeAttempt(ctx, lease, input, hashes, "policy")
}

func (w *Worker) releaseRetry(
	ctx context.Context,
	token LeaseToken,
	input *RequestInput,
	failure SafeFailure,
	attempt int64,
) error {
	delay, err := RetryDelay(RetryInput{
		Attempt: int(attempt), Base: time.Second, Maximum: httpretry.ProviderMaxRetryAfter,
		Now: w.options.Clock().UTC(), Jitter: w.options.Jitter,
	})
	if err != nil {
		return err
	}
	if delay <= 0 {
		delay = time.Millisecond
	}
	next := w.options.Clock().UTC().Add(delay)
	release := WorkRelease{Outcome: "retry", Failure: &failure, NextActionAt: &next}
	if input != nil {
		release.PersonRevision = input.PersonRevision
	}
	return w.work.ReleaseWork(ctx, token, release)
}

func activeAttemptCount(lease WorkLease) int64 {
	if lease.ActiveAttempt == nil || lease.ActiveAttempt.AttemptCount <= 0 {
		return 1
	}
	return lease.ActiveAttempt.AttemptCount
}

func safeFailure(class FailureClass, status int, requestID, message string) SafeFailure {
	if len(requestID) > 512 {
		requestID = requestID[:512]
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return SafeFailure{
		Class: class, HTTPStatus: status,
		ProviderRequestID: requestID, Message: message,
	}
}
