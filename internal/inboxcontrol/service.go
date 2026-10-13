package inboxcontrol

import (
	"context"
	"crypto/rand"
	"errors"
	"time"
)

var (
	ErrDenied         = errors.New("inbox operation denied")
	ErrUnavailable    = errors.New("inbox operation unavailable")
	ErrPlanChanged    = errors.New("inbox state changed since preview")
	ErrOutcomeUnknown = errors.New("inbox provider outcome unknown; inspect the receipt")
	ErrReconcileOnly  = errors.New("inbox provider state verified; local reconciliation required")
	// ErrNoWrite may be reported only when the provider proves no write occurred.
	ErrNoWrite = errors.New("inbox provider rejected the operation without a write")
)

// Principal comes exclusively from daemon authentication. Owner is never a
// request field; delegates can inspect only receipts belonging to their ID.
type Principal struct {
	ID    string
	Owner bool
}

// DispatchResult carries an authoritative mapping from the dispatch response,
// never a heuristic lookup. A successful response still needs independent GET
// readback.
type DispatchResult struct {
	Target *Target
	Folder *Folder
}

// Provider methods operate on a Store-resolved binding. Observe, Preview and
// Folders are read-only. Dispatch sends once; neither method acquires a daemon
// gate or source lease. Verify proves the requested delta and preserved state.
type Provider interface {
	Observe(ctx context.Context, request Request) (State, error)
	Preview(ctx context.Context, request Request, before State) (State, error)
	Dispatch(ctx context.Context, request Request, before State) (DispatchResult, error)
	Verify(request Request, before State, projected State, after State) error
	Folders(ctx context.Context, source SourceIdentity) ([]Folder, error)
}

type Result struct {
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	Before       *State        `json:"before,omitempty"`
	Projected    *State        `json:"projected,omitempty"`
	After        *State        `json:"after,omitempty"`
	PreviewToken string        `json:"preview_token,omitempty"`
	ExpiresAt    time.Time     `json:"expires_at,omitzero"`
	Receipt      *Receipt      `json:"receipt,omitempty"`
	Folders      []Folder      `json:"folders,omitempty"`
}

// Service owns the entire mutation boundary. Authorize must check current
// source/action grants, including inbox.read; Resolve must validate every
// supplied binding against Store and configured provider account identity.
// AcquireSource excludes sync. ReconcileState updates only verified evidence.
type Service struct {
	Ledger         Ledger
	Key            []byte
	Now            func() time.Time
	Authorize      func(context.Context, Principal, Request) error
	Resolve        func(context.Context, Request) (Provider, error)
	AcquireSource  func(context.Context, int64) (func(), error)
	ReconcileState func(context.Context, State, State) error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) authorize(ctx context.Context, principal Principal, request Request) error {
	if !validIdentity(principal.ID) || s.Authorize == nil {
		return ErrDenied
	}
	if err := s.Authorize(ctx, principal, request); err != nil {
		return ErrDenied
	}
	return nil
}

// Control reauthorizes existing receipts before checking preview freshness.
// Unknown outcomes are returned as receipts and are never dispatched again.
func (s *Service) Control(ctx context.Context, request Request, principal Principal, acquireWrite func(context.Context) (func(), error)) (*Result, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if !validIdentity(principal.ID) {
		return nil, ErrDenied
	}
	if s.Ledger == nil || s.Resolve == nil || len(s.Key) < 32 {
		return nil, ErrInternal
	}
	if request.Operation == OpReceiptGet || request.Operation == OpReconcile {
		return s.controlReceipt(ctx, request, principal, acquireWrite)
	}
	mutation := request.Operation.IsMutation()
	intentHash, err := IntentFingerprint(request)
	if err != nil {
		return nil, err
	}
	if mutation && !request.DryRun {
		receipt, err := s.Ledger.LookupInboxReceipt(ctx, principal.ID, requestSourceID(request), request.IdempotencyKey)
		if err != nil {
			return nil, ErrInternal
		}
		if receipt != nil {
			return s.replay(ctx, receipt, intentHash, principal)
		}
	}
	if err := s.authorize(ctx, principal, request); err != nil {
		return nil, err
	}
	if !mutation || request.DryRun {
		provider, err := s.Resolve(ctx, request)
		if err != nil {
			return nil, safePreflightError(err)
		}
		if provider == nil {
			return nil, ErrUnavailable
		}
		defer closeProvider(provider)
		if request.Operation == OpGetCapabilities {
			return s.readCapabilities(ctx, provider, request, principal)
		}
		if request.Operation == OpListFolders {
			folders, err := provider.Folders(ctx, *request.Source)
			if err != nil {
				return nil, safePreflightError(err)
			}
			return &Result{Folders: folders}, nil
		}
		before, err := provider.Observe(ctx, request)
		if err != nil {
			return nil, safePreflightError(err)
		}
		if !observationMatchesRequest(before, request) {
			return nil, ErrUnavailable
		}
		result := &Result{Before: &before}
		if !mutation {
			return result, nil
		}
		projected, err := provider.Preview(ctx, request, before)
		if err != nil {
			return nil, safePreflightError(err)
		}
		stateHash, err := planFingerprint(before, projected)
		if err != nil {
			return nil, ErrUnavailable
		}
		now := s.now()
		result.Projected, result.ExpiresAt = &projected, now.Add(previewTTL)
		result.PreviewToken, err = SignPreview(s.Key, PreviewClaims{PrincipalID: principal.ID, IntentHash: intentHash, StateHash: stateHash, IssuedAt: now, ExpiresAt: result.ExpiresAt})
		if err != nil {
			return nil, ErrInternal
		}
		return result, nil
	}
	return s.execute(ctx, request, principal, intentHash, acquireWrite)
}

func (s *Service) acquire(ctx context.Context, sourceID int64, gate func(context.Context) (func(), error)) (func(), error) {
	if gate == nil || s.AcquireSource == nil {
		return nil, ErrInternal
	}
	done, err := gate(ctx)
	if err != nil {
		return nil, err
	}
	if done == nil {
		return nil, ErrInternal
	}
	release, err := s.AcquireSource(ctx, sourceID)
	if err != nil {
		done()
		return nil, ErrUnavailable
	}
	if release == nil {
		done()
		return nil, ErrInternal
	}
	return func() { release(); done() }, nil
}

func (s *Service) execute(ctx context.Context, request Request, principal Principal, intentHash string, gate func(context.Context) (func(), error)) (*Result, error) {
	done, err := s.acquire(ctx, requestSourceID(request), gate)
	if err != nil {
		return nil, err
	}
	defer done()
	// A competing claim could have completed while this request waited.
	existing, err := s.Ledger.LookupInboxReceipt(ctx, principal.ID, requestSourceID(request), request.IdempotencyKey)
	if err != nil {
		return nil, ErrInternal
	}
	if existing != nil {
		return s.replay(ctx, existing, intentHash, principal)
	}
	if err := s.authorize(ctx, principal, request); err != nil {
		return nil, err
	}
	provider, err := s.Resolve(ctx, request)
	if err != nil {
		return nil, safePreflightError(err)
	}
	if provider == nil {
		return nil, ErrUnavailable
	}
	defer closeProvider(provider)
	if imapMoveRequest(request) {
		if _, ok := s.Ledger.(ReceiptMappingLedger); !ok {
			return nil, ErrUnavailable
		}
		if _, ok := provider.(MoveSourceVerifier); !ok {
			return nil, ErrUnavailable
		}
	}
	before, err := provider.Observe(ctx, request)
	if err != nil {
		return nil, safePreflightError(err)
	}
	if !observationMatchesRequest(before, request) {
		return nil, ErrUnavailable
	}
	stateHash, err := SemanticFingerprint(before)
	if err != nil {
		return nil, ErrUnavailable
	}
	expectedHash, err := SemanticFingerprint(*request.Expected)
	if err != nil || expectedHash != stateHash {
		return nil, ErrPlanChanged
	}
	projected, err := provider.Preview(ctx, request, before)
	if err != nil {
		return nil, safePreflightError(err)
	}
	planHash, err := planFingerprint(before, projected)
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := VerifyPreview(s.Key, request.PreviewToken, principal.ID, intentHash, planHash, s.now()); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, principal, request); err != nil {
		return nil, err
	}
	intent := request
	intent.DryRun, intent.Expected, intent.PreviewToken, intent.IdempotencyKey = true, nil, "", ""
	receipt, claimed, err := s.Ledger.PrepareInboxReceipt(ctx, Receipt{ID: rand.Text(), PrincipalID: principal.ID, SourceID: requestSourceID(request), IdempotencyKey: request.IdempotencyKey, IntentHash: intentHash, StateHash: stateHash, Intent: intent, Before: before, Projected: &projected, Status: StatusPrepared, CreatedAt: s.now()})
	if err != nil {
		return nil, ErrInternal
	}
	if !claimed {
		return s.replay(ctx, receipt, intentHash, principal)
	}
	if err := s.Ledger.MarkInboxDispatching(ctx, receipt.ID); err != nil {
		return &Result{Receipt: receipt}, ErrInternal
	}
	result := &Result{Before: &before, Projected: &projected, Receipt: receipt}
	dispatchRequest := request
	if imapMoveRequest(request) && len(projected.Folders) == 1 {
		destination := projected.Folders[0]
		dispatchRequest.ResolvedFolder = &destination
	}
	dispatch, dispatchErr := provider.Dispatch(ctx, dispatchRequest, before)
	readRequest := intent
	var mapping *State
	if dispatch.Folder != nil {
		if dispatch.Target != nil || request.Operation != OpCreateFolder || request.Source == nil || request.Destination == nil || !validIdentity(dispatch.Folder.ID) || !validIdentity(dispatch.Folder.Name) || dispatch.Folder.Name != request.Destination.Name || dispatch.Folder.ParentID != request.Destination.ParentID {
			return s.finishDetached(ctx, result, StatusUnknown, nil, "mapping-unverified", ErrOutcomeUnknown)
		}
		folder := *dispatch.Folder
		readRequest.ResolvedFolder = &folder
		mapping = &State{Source: before.Source, ProvisionedFolder: &folder, ObservedAt: s.now()}
	}
	if dispatch.Target != nil {
		if !validDispatchMapping(request, before, projected, *dispatch.Target) {
			return s.finishDetached(ctx, result, StatusUnknown, nil, "mapping-unverified", ErrOutcomeUnknown)
		}
		// Preserve the dispatch response independently of provider readback.
		mapping = &State{Target: *dispatch.Target, ObservedAt: s.now()}
		readRequest.Target = dispatch.Target
		if imapMoveRequest(request) {
			mappingLedger, ok := s.Ledger.(ReceiptMappingLedger)
			if !ok {
				return s.finishDetached(ctx, result, StatusUnknown, nil, "mapping-persist-failed", ErrOutcomeUnknown)
			}
			mappingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := mappingLedger.RecordInboxReceiptMapping(mappingCtx, receipt.ID, *mapping)
			cancel()
			if err != nil {
				return s.finishDetached(ctx, result, StatusUnknown, nil, "mapping-persist-failed", ErrOutcomeUnknown)
			}
		}
	}
	if dispatchErr != nil {
		if errors.Is(dispatchErr, ErrNoWrite) && mapping == nil {
			return s.finishDetached(ctx, result, StatusFailed, nil, "provider-rejected", ErrNoWrite)
		}
		return s.finishDetached(ctx, result, StatusUnknown, mapping, "dispatch-unverified", ErrOutcomeUnknown)
	}
	after, err := provider.Observe(ctx, readRequest)
	if err != nil || !observationMatchesRequest(after, readRequest) {
		// Preserve dispatch evidence even when GET fails. Its markers are
		// unknown and cannot be used as successful readback.
		if dispatch.Target != nil {
			mapping = &State{Target: *dispatch.Target, ObservedAt: s.now()}
		}
		return s.finishDetached(ctx, result, StatusUnknown, mapping, "readback-unverified", ErrOutcomeUnknown)
	}
	if err := provider.Verify(readRequest, before, projected, after); err != nil {
		return s.finishDetached(ctx, result, StatusUnknown, &after, "delta-unverified", ErrOutcomeUnknown)
	}
	if err := verifyMovedSource(ctx, provider, before, after); err != nil {
		return s.finishDetached(ctx, result, StatusUnknown, &after, "source-unverified", ErrOutcomeUnknown)
	}
	result.After = &after
	// Local reconciliation and durable receipt settlement have independent
	// deadlines, even after cancellation or a slow provider readback.
	completionBase := ctx
	if imapMoveRequest(request) && mapping != nil {
		completionBase = context.WithValue(ctx, receiptReconcileIDKey{}, receipt.ID)
	}
	completion, cancel := context.WithTimeout(context.WithoutCancel(completionBase), 5*time.Second)
	defer cancel()
	if s.ReconcileState == nil || s.ReconcileState(completion, before, after) != nil {
		return s.finishDetached(ctx, result, StatusReconcileOnly, &after, "local-reconcile-failed", ErrReconcileOnly)
	}
	return s.finishDetached(ctx, result, StatusVerified, &after, "", nil)
}

func (s *Service) finishDetached(ctx context.Context, result *Result, status Status, after *State, code string, outcome error) (*Result, error) {
	completion, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.finish(completion, result, status, after, code, outcome)
}

func (s *Service) finish(ctx context.Context, result *Result, status Status, after *State, code string, outcome error) (*Result, error) {
	if err := s.Ledger.FinishInboxReceipt(ctx, result.Receipt.ID, status, after, code); err != nil {
		if status == StatusVerified {
			_ = s.Ledger.FinishInboxReceipt(ctx, result.Receipt.ID, StatusReconcileOnly, after, "receipt-finalization-failed")
			outcome = ErrReconcileOnly
		} else {
			outcome = ErrOutcomeUnknown
		}
	}
	stored, err := s.Ledger.GetInboxReceipt(ctx, result.Receipt.ID)
	if err != nil || stored == nil {
		return result, ErrOutcomeUnknown
	}
	result.Receipt = stored
	return result, outcome
}

func (s *Service) replay(ctx context.Context, receipt *Receipt, intentHash string, principal Principal) (*Result, error) {
	if receipt == nil || !validIdentity(principal.ID) {
		return nil, ErrDenied
	}
	if !principal.Owner && receipt.PrincipalID != principal.ID {
		return nil, ErrDenied
	}
	if err := s.authorize(receiptAuthorityContext(ctx, receipt), principal, receipt.Intent); err != nil {
		return nil, err
	}
	if receipt.IntentHash != intentHash {
		return nil, ErrConflict
	}
	return &Result{Receipt: receipt, Before: &receipt.Before, Projected: receipt.Projected, After: receipt.After}, nil
}

func (s *Service) controlReceipt(ctx context.Context, request Request, principal Principal, gate func(context.Context) (func(), error)) (*Result, error) {
	receipt, err := s.Ledger.GetInboxReceipt(ctx, request.ReceiptID)
	if err != nil {
		return nil, ErrInternal
	}
	if receipt == nil {
		return nil, ErrDenied
	}
	result, err := s.replay(ctx, receipt, receipt.IntentHash, principal)
	if err != nil || request.Operation == OpReceiptGet || receipt.Status == StatusVerified || receipt.Status == StatusFailed {
		return result, err
	}
	done, err := s.acquire(ctx, receipt.SourceID, gate)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := s.authorize(receiptAuthorityContext(ctx, receipt), principal, receipt.Intent); err != nil {
		return nil, err
	}
	read := receipt.Intent
	if receipt.Projected == nil {
		return result, ErrUnavailable
	}
	if receipt.After != nil && receipt.After.Target != (Target{}) {
		read.Target = &receipt.After.Target
	}
	if read.Operation == OpCreateFolder {
		if receipt.After != nil && receipt.After.ProvisionedFolder != nil && receipt.After.ProvisionedFolder.ID != "" {
			read.ResolvedFolder = receipt.After.ProvisionedFolder
		} else if receipt.Projected.ProvisionedFolder != nil && receipt.Projected.ProvisionedFolder.ID != "" {
			read.ResolvedFolder = receipt.Projected.ProvisionedFolder
		} else {
			// A label with the requested name is insufficient dispatch evidence.
			return result, ErrUnavailable
		}
	}
	resolveContext := ctx
	if imapMoveRequest(receipt.Intent) && read.Target != nil && *read.Target != receipt.Before.Target {
		if receipt.After == nil || !validDispatchMapping(receipt.Intent, receipt.Before, *receipt.Projected, *read.Target) {
			return result, ErrOutcomeUnknown
		}
		resolveContext = context.WithValue(ctx, receiptMoveTargetKey{}, *read.Target)
	}
	provider, err := s.Resolve(resolveContext, read)
	if err != nil || provider == nil {
		return result, ErrUnavailable
	}
	defer closeProvider(provider)
	after, err := provider.Observe(ctx, read)
	if err != nil || !observationMatchesRequest(after, read) {
		return result, ErrOutcomeUnknown
	}
	if err := provider.Verify(read, receipt.Before, *receipt.Projected, after); err != nil {
		return result, ErrOutcomeUnknown
	}
	if err := verifyMovedSource(ctx, provider, receipt.Before, after); err != nil {
		return result, ErrOutcomeUnknown
	}
	// Local reconciliation and durable receipt settlement have independent
	// deadlines, even after cancellation or a slow provider readback.
	completionBase := resolveContext
	if imapMoveRequest(receipt.Intent) && read.Target != nil && *read.Target != receipt.Before.Target {
		completionBase = context.WithValue(resolveContext, receiptReconcileIDKey{}, receipt.ID)
	}
	completion, cancel := context.WithTimeout(context.WithoutCancel(completionBase), 5*time.Second)
	defer cancel()
	if s.ReconcileState == nil || s.ReconcileState(completion, receipt.Before, after) != nil {
		return s.finishDetached(ctx, result, StatusReconcileOnly, &after, "local-reconcile-failed", ErrReconcileOnly)
	}
	result.After = &after
	return s.finishDetached(ctx, result, StatusVerified, &after, "", nil)
}

func requestSourceID(request Request) int64 {
	if request.Target != nil {
		return request.Target.SourceID
	}
	if request.Source != nil {
		return request.Source.SourceID
	}
	return 0
}

func observationMatchesRequest(state State, request Request) bool {
	if state.ObservedAt.IsZero() {
		return false
	}
	if request.Target != nil {
		return state.Target == *request.Target && state.Source == (SourceIdentity{})
	}
	return request.Source != nil && state.Source == *request.Source && state.Target == (Target{})
}

func sameItemIdentity(before, after Target) bool {
	if after.Validate() != nil {
		return false
	}
	return before == after
}

func safePreflightError(err error) error {
	for _, safe := range []error{ErrDenied, ErrInvalid, ErrPlanChanged, ErrConflict, ErrUnavailable} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return ErrUnavailable
}

// Destination resolution and projected state are authenticated along with
// the observation. A configured Archive change cannot reuse an old preview.
func planFingerprint(before, projected State) (string, error) {
	beforeHash, err := SemanticFingerprint(before)
	if err != nil {
		return "", err
	}
	projectedHash, err := SemanticFingerprint(projected)
	if err != nil {
		return "", err
	}
	return fingerprint(struct{ Before, Projected string }{beforeHash, projectedHash})
}

func closeProvider(provider Provider) {
	if closer, ok := provider.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// MoveSourceVerifier independently proves the original UID disappeared.
type MoveSourceVerifier interface {
	VerifyMoveSource(ctx context.Context, target Target) error
}

func imapMoveRequest(request Request) bool {
	return request.Target != nil && request.Target.SourceType == sourceTypeIMAP && (request.Operation == OpMove || request.Operation == OpArchive || request.Operation == OpUnarchive)
}

func validDispatchMapping(request Request, before, projected State, target Target) bool {
	if target.Validate() != nil {
		return false
	}
	if !imapMoveRequest(request) {
		return sameItemIdentity(before.Target, target)
	}
	if len(projected.Folders) != 1 {
		return false
	}
	destination := projected.Folders[0]
	if destination.ID == "" || destination.UIDValidity == 0 || destination.ParentID != "" || target.Mailbox != destination.ID || target.UIDValidity != destination.UIDValidity || target.UID == 0 {
		return false
	}
	origin := target
	origin.Mailbox, origin.UIDValidity, origin.UID = before.Target.Mailbox, before.Target.UIDValidity, before.Target.UID
	return origin == before.Target
}

func verifyMovedSource(ctx context.Context, provider Provider, before, after State) error {
	if before.Target.SourceType != sourceTypeIMAP || before.Target == after.Target {
		return nil
	}
	verifier, ok := provider.(MoveSourceVerifier)
	if !ok {
		return ErrOutcomeUnknown
	}
	return verifier.VerifyMoveSource(ctx, before.Target)
}

type receiptMoveTargetKey struct{}

// ReceiptMoveTarget is an internal resolver hint from durable dispatch mapping.
// It never replaces archive item/source/receipt authority checks.
func ReceiptMoveTarget(ctx context.Context) (Target, bool) {
	target, ok := ctx.Value(receiptMoveTargetKey{}).(Target)
	return target, ok
}

// A durable validated mapping permits receipt authorization on the original
// live item after local reconciliation has retired only its origin membership.
// Principal/intent checks precede this context; current grants remain required.
type receiptAuthorityTargetKey struct{}

func receiptAuthorityContext(ctx context.Context, receipt *Receipt) context.Context {
	if receipt == nil || receipt.Projected == nil || receipt.After == nil || !imapMoveRequest(receipt.Intent) || *receipt.Intent.Target != receipt.Before.Target || receipt.SourceID != receipt.Before.Target.SourceID || receipt.Before.Target.Validate() != nil || receipt.Projected.Target != receipt.Before.Target || receipt.After.Target == receipt.Before.Target || !validDispatchMapping(receipt.Intent, receipt.Before, *receipt.Projected, receipt.After.Target) {
		return ctx
	}
	return context.WithValue(ctx, receiptAuthorityTargetKey{}, receipt.Before.Target)
}

// ReceiptAuthorityTarget is an internal hint, never a request field.
func ReceiptAuthorityTarget(ctx context.Context) (Target, bool) {
	target, ok := ctx.Value(receiptAuthorityTargetKey{}).(Target)
	return target, ok
}

// Only Service can attach an authenticated receipt identity to reconciliation.
// Store independently reloads its append-only mapping inside the transaction.
type receiptReconcileIDKey struct{}

func ReceiptReconcileID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(receiptReconcileIDKey{}).(string)
	return id, ok && validIdentity(id)
}
