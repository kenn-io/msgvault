package identityops

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

const (
	SignalIsFromMe      = "is_from_me"
	SignalProviderAlias = "provider-alias"
	SignalSentFolder    = "sent-folder"
	SignalSentLabel     = "sent-label"

	classificationConfirmed = "confirmed"
	classificationStrong    = "strong"
	classificationWeak      = "weak"

	defaultDiscoveryPageSize = 1000
	maxDiscoveryPageSize     = 5000
)

// DiscoveryStore is the source-resolution, metadata-scan, and bounded-write
// surface used by identity discovery.
type DiscoveryStore interface {
	Store
	CountIdentityDiscoveryMessagesContext(ctx context.Context, sourceID int64) (int64, error)
	ScanIdentityDiscoveryPageContext(ctx context.Context, sourceID, afterID int64, limit int) (store.IdentityDiscoveryPage, error)
	ScanIdentityObservationsForSourceMessageIDsContext(ctx context.Context, sourceID int64, sourceMessageIDs []string) ([]store.IdentityObservation, error)
	AddAccountIdentitiesBatchContext(ctx context.Context, sourceID int64, candidates []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error)
	MergeConfirmedAccountIdentitySignalsContext(ctx context.Context, sourceID int64, candidates []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error)
}

// ExternalEvidenceStore is the bounded identity-write service used by
// provider refreshes that do not need to rescan archived messages.
type ExternalEvidenceStore interface {
	ListAccountIdentities(sourceID int64) ([]store.AccountIdentity, error)
	AddAccountIdentitiesBatchContext(ctx context.Context, sourceID int64, candidates []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error)
}

type Candidate struct {
	Identifier           string    `json:"identifier"`
	NormalizedIdentifier string    `json:"normalized_identifier"`
	Classification       string    `json:"classification" enum:"confirmed,strong,weak"`
	AlreadyConfirmed     bool      `json:"already_confirmed"`
	Signals              []string  `json:"signals"`
	ProviderStates       []string  `json:"provider_states" nullable:"false"`
	SentMessageCount     int64     `json:"sent_message_count"`
	ReceivedMessageCount int64     `json:"received_message_count"`
	FirstSeenAt          time.Time `json:"first_seen_at"`
	LastSeenAt           time.Time `json:"last_seen_at"`

	strongSignals []string
}

type RejectedCandidate struct {
	Identifier string `json:"identifier"`
	Reason     string `json:"reason"`
}

type DiscoverRequest struct {
	SourceSelector

	Apply    bool     `json:"apply,omitempty"`
	Provider bool     `json:"provider,omitempty"`
	Confirm  []string `json:"confirm,omitempty"`
	PageSize int      `json:"page_size,omitempty"`
}

// ExternalEvidence is provider- or import-supplied identity evidence. State is
// reporting metadata only; Strong controls whether the signal may be applied.
type ExternalEvidence struct {
	Identifier     string
	Signal         string
	State          string
	Strong         bool
	RejectedReason string
}

type DiscoverProgress struct {
	Done       int64 `json:"done"`
	Total      int64 `json:"total"`
	Candidates int   `json:"candidates"`
}

type DiscoverResult struct {
	Account         string                              `json:"account"`
	SourceID        int64                               `json:"source_id"`
	SourceType      string                              `json:"source_type"`
	ScannedMessages int64                               `json:"scanned_messages"`
	Candidates      []Candidate                         `json:"candidates"`
	Rejected        []RejectedCandidate                 `json:"rejected"`
	Applied         []store.IdentityConfirmationOutcome `json:"applied"`
}

// DiscoverError is the stable, sanitized terminal failure carried by a
// discovery stream after progress has already committed the NDJSON response.
// Provider and storage details remain on the daemon side of the boundary.
type DiscoverError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *DiscoverError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

type DiscoverEvent struct {
	Type     string            `json:"type" enum:"progress,result,error"`
	Progress *DiscoverProgress `json:"progress,omitempty"`
	Result   *DiscoverResult   `json:"result,omitempty"`
	Error    *DiscoverError    `json:"error,omitempty"`
}

type candidateAccumulator struct {
	identifier       string
	normalized       string
	signals          map[string]struct{}
	strongSignals    map[string]struct{}
	existingSignals  map[string]struct{}
	alreadyConfirmed bool
	sentMessages     map[int64]struct{}
	receivedMessages map[int64]struct{}
	firstSeenAt      time.Time
	lastSeenAt       time.Time
}

type classifiedDiscovery struct {
	candidates    []Candidate
	rejected      []RejectedCandidate
	confirmations []store.IdentityConfirmation
}

// Discover scans one source completely before optionally applying strong or
// explicitly confirmed evidence. No write occurs during the scan.
func Discover(
	ctx context.Context,
	st DiscoveryStore,
	req DiscoverRequest,
	progress func(DiscoverProgress) error,
) (DiscoverResult, error) {
	return DiscoverWithExternalEvidence(ctx, st, req, nil, progress)
}

// DiscoverWithExternalEvidence merges normalized external evidence before
// deriving the optional write plan, so preview and apply expose one result.
func DiscoverWithExternalEvidence(
	ctx context.Context,
	st DiscoveryStore,
	req DiscoverRequest,
	evidence []ExternalEvidence,
	progress func(DiscoverProgress) error,
) (DiscoverResult, error) {
	if err := ctx.Err(); err != nil {
		return DiscoverResult{}, err
	}
	if len(req.Confirm) > 0 && !req.Apply {
		return DiscoverResult{}, opserr.Invalid(errors.New("explicit confirmation requires apply"))
	}
	pageSize, err := discoveryPageSize(req.PageSize)
	if err != nil {
		return DiscoverResult{}, err
	}
	src, err := ResolveSource(st, req.SourceSelector)
	if err != nil {
		return DiscoverResult{}, err
	}
	total, err := st.CountIdentityDiscoveryMessagesContext(ctx, src.ID)
	if err != nil {
		return DiscoverResult{}, opserr.Internal(fmt.Errorf("count identity discovery messages: %w", err))
	}
	existing, err := listAccountIdentities(st, src.ID)
	if err != nil {
		return DiscoverResult{}, err
	}

	accumulators := make(map[string]*candidateAccumulator)
	rejected := make(map[RejectedCandidate]struct{})
	done, err := scanIdentityObservationPages(
		ctx, st, src.ID, pageSize, accumulators, rejected,
		func(done int64, candidates int) error {
			if progress == nil {
				return nil
			}
			return progress(DiscoverProgress{Done: done, Total: total, Candidates: candidates})
		},
	)
	if err != nil {
		return DiscoverResult{}, err
	}

	classified := classifyDiscovery(accumulators, rejected, existing)
	result := DiscoverResult{
		Account:         src.Identifier,
		SourceID:        src.ID,
		SourceType:      src.SourceType,
		ScannedMessages: done,
		Candidates:      classified.candidates,
		Rejected:        classified.rejected,
		Applied:         []store.IdentityConfirmationOutcome{},
	}
	seedExistingExternalCandidates(&result, evidence, existing)
	MergeExternalEvidence(&result, evidence)
	if !req.Apply {
		return result, nil
	}
	classified.candidates = result.Candidates
	classified.confirmations = strongConfirmations(result.Candidates, existing)
	if err := addExplicitWeakConfirmations(&classified, req.Confirm); err != nil {
		return DiscoverResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return DiscoverResult{}, err
	}
	if len(classified.confirmations) == 0 {
		return result, nil
	}
	result.Applied, err = st.AddAccountIdentitiesBatchContext(ctx, src.ID, classified.confirmations)
	if err != nil {
		return result, err
	}
	return result, nil
}

func seedExistingExternalCandidates(
	result *DiscoverResult,
	evidence []ExternalEvidence,
	existing []store.AccountIdentity,
) {
	if len(evidence) == 0 || len(existing) == 0 {
		return
	}
	candidateNormalized := make(map[string]struct{}, len(result.Candidates))
	for _, candidate := range result.Candidates {
		candidateNormalized[candidate.NormalizedIdentifier] = struct{}{}
	}
	existingByNormalized := make(map[string]store.AccountIdentity, len(existing))
	for _, identity := range existing {
		normalized := store.NormalizeIdentifierForCompare(strings.TrimSpace(identity.Address))
		existingByNormalized[normalized] = identity
	}
	seeded := make(map[string]struct{})
	for _, item := range evidence {
		if item.RejectedReason != "" {
			continue
		}
		identifier, reason := validateDiscoveredIdentifier(item.Identifier)
		if reason != "" {
			continue
		}
		normalized := store.NormalizeIdentifierForCompare(identifier)
		if _, present := candidateNormalized[normalized]; present {
			continue
		}
		if _, present := seeded[normalized]; present {
			continue
		}
		identity, confirmed := existingByNormalized[normalized]
		if !confirmed {
			continue
		}
		result.Candidates = append(result.Candidates, Candidate{
			Identifier:           strings.TrimSpace(identity.Address),
			NormalizedIdentifier: normalized,
			Classification:       classificationConfirmed,
			AlreadyConfirmed:     true,
			Signals:              SplitSignalSet(identity.SourceSignal),
			ProviderStates:       []string{},
			strongSignals:        []string{},
		})
		seeded[normalized] = struct{}{}
	}
}

// MergeExternalEvidence case-folds concrete mailbox addresses, preserves
// existing confirmation metadata, and deterministically merges signals and
// rejections. It never removes or unconfirms an identity based on State.
func MergeExternalEvidence(result *DiscoverResult, evidence []ExternalEvidence) {
	if result == nil || len(evidence) == 0 {
		return
	}

	candidates := make(map[string]*Candidate, len(result.Candidates)+len(evidence))
	for i := range result.Candidates {
		candidate := result.Candidates[i]
		normalized := candidate.NormalizedIdentifier
		if normalized == "" {
			normalized = store.NormalizeIdentifierForCompare(strings.TrimSpace(candidate.Identifier))
			candidate.NormalizedIdentifier = normalized
		}
		candidate.Signals = sortedUnique(candidate.Signals)
		candidate.ProviderStates = sortedUnique(candidate.ProviderStates)
		candidate.strongSignals = sortedUnique(candidate.strongSignals)
		copyCandidate := candidate
		candidates[normalized] = &copyCandidate
	}
	rejected := make(map[RejectedCandidate]struct{}, len(result.Rejected)+len(evidence))
	for _, candidate := range result.Rejected {
		rejected[candidate] = struct{}{}
	}

	ordered := append([]ExternalEvidence(nil), evidence...)
	sort.SliceStable(ordered, func(i, j int) bool {
		leftIdentifier := strings.TrimSpace(ordered[i].Identifier)
		rightIdentifier := strings.TrimSpace(ordered[j].Identifier)
		leftNormalized := store.NormalizeIdentifierForCompare(leftIdentifier)
		rightNormalized := store.NormalizeIdentifierForCompare(rightIdentifier)
		if leftNormalized != rightNormalized {
			return leftNormalized < rightNormalized
		}
		if leftIdentifier != rightIdentifier {
			return leftIdentifier < rightIdentifier
		}
		if ordered[i].Signal != ordered[j].Signal {
			return ordered[i].Signal < ordered[j].Signal
		}
		if ordered[i].State != ordered[j].State {
			return ordered[i].State < ordered[j].State
		}
		if ordered[i].Strong != ordered[j].Strong {
			return ordered[i].Strong
		}
		return ordered[i].RejectedReason < ordered[j].RejectedReason
	})

	for _, item := range ordered {
		identifier := strings.TrimSpace(item.Identifier)
		if item.RejectedReason != "" {
			rejected[RejectedCandidate{Identifier: identifier, Reason: item.RejectedReason}] = struct{}{}
			continue
		}
		validated, reason := validateDiscoveredIdentifier(identifier)
		if reason != "" {
			rejected[RejectedCandidate{Identifier: identifier, Reason: reason}] = struct{}{}
			continue
		}
		normalized := store.NormalizeIdentifierForCompare(validated)
		candidate, ok := candidates[normalized]
		if !ok {
			candidate = &Candidate{
				Identifier:           validated,
				NormalizedIdentifier: normalized,
				Classification:       classificationWeak,
				Signals:              []string{},
				ProviderStates:       []string{},
				strongSignals:        []string{},
			}
			candidates[normalized] = candidate
		}
		if signal := strings.TrimSpace(item.Signal); signal != "" {
			candidate.Signals = appendUnique(candidate.Signals, signal)
			if item.Strong {
				candidate.strongSignals = appendUnique(candidate.strongSignals, signal)
			}
		}
		if state := strings.TrimSpace(item.State); state != "" {
			candidate.ProviderStates = appendUnique(candidate.ProviderStates, state)
		}
		switch {
		case candidate.AlreadyConfirmed || candidate.Classification == classificationConfirmed:
			candidate.AlreadyConfirmed = true
			candidate.Classification = classificationConfirmed
		case item.Strong || candidate.Classification == classificationStrong:
			candidate.Classification = classificationStrong
		default:
			candidate.Classification = classificationWeak
		}
	}

	result.Candidates = make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.Signals = sortedUnique(candidate.Signals)
		candidate.ProviderStates = sortedUnique(candidate.ProviderStates)
		candidate.strongSignals = sortedUnique(candidate.strongSignals)
		result.Candidates = append(result.Candidates, *candidate)
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		left, right := result.Candidates[i], result.Candidates[j]
		if left.NormalizedIdentifier == right.NormalizedIdentifier {
			return left.Identifier < right.Identifier
		}
		return left.NormalizedIdentifier < right.NormalizedIdentifier
	})
	result.Rejected = make([]RejectedCandidate, 0, len(rejected))
	for candidate := range rejected {
		result.Rejected = append(result.Rejected, candidate)
	}
	sort.Slice(result.Rejected, func(i, j int) bool {
		if result.Rejected[i].Identifier == result.Rejected[j].Identifier {
			return result.Rejected[i].Reason < result.Rejected[j].Reason
		}
		return result.Rejected[i].Identifier < result.Rejected[j].Identifier
	})
}

// ApplyExternalEvidence confirms only strong normalized evidence through the
// store's bounded batch service. Weak and rejected rows never write.
func ApplyExternalEvidence(
	ctx context.Context,
	st ExternalEvidenceStore,
	sourceID int64,
	evidence []ExternalEvidence,
) ([]store.IdentityConfirmationOutcome, error) {
	if sourceID <= 0 {
		return nil, opserr.Invalid(errors.New("source ID must be positive"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	existing, err := listAccountIdentities(st, sourceID)
	if err != nil {
		return nil, err
	}
	result := DiscoverResult{SourceID: sourceID}
	MergeExternalEvidence(&result, evidence)
	confirmations := strongConfirmations(result.Candidates, existing)
	if len(confirmations) == 0 {
		return []store.IdentityConfirmationOutcome{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outcomes, err := st.AddAccountIdentitiesBatchContext(ctx, sourceID, confirmations)
	if err != nil {
		return outcomes, err
	}
	return outcomes, nil
}

// DiscoverStrongForSourceMessageIDs refreshes one bounded ingestion batch's
// authoritative From evidence into the source's already-confirmed identities.
// It never confirms a first-time identity: placement in Sent is evidence about
// a message, and claiming an address from it during sync would let one
// mis-filed message silently take ownership. New identities are confirmed only
// through the reviewed discovery path.
func DiscoverStrongForSourceMessageIDs(
	ctx context.Context,
	st DiscoveryStore,
	sourceID int64,
	sourceMessageIDs []string,
) ([]store.IdentityConfirmationOutcome, error) {
	if sourceID <= 0 {
		return nil, opserr.Invalid(errors.New("source ID must be positive"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(sourceMessageIDs) == 0 {
		return []store.IdentityConfirmationOutcome{}, nil
	}
	existing, err := listAccountIdentities(st, sourceID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return []store.IdentityConfirmationOutcome{}, nil
	}
	observations, err := st.ScanIdentityObservationsForSourceMessageIDsContext(
		ctx, sourceID, sourceMessageIDs,
	)
	if err != nil {
		return nil, opserr.Internal(fmt.Errorf("scan identity observations for source message IDs: %w", err))
	}
	accumulators := make(map[string]*candidateAccumulator)
	rejected := make(map[RejectedCandidate]struct{})
	mergeIdentityObservations(accumulators, rejected, observations)
	return mergeConfirmedIdentitySignals(ctx, st, sourceID, existing, accumulators, rejected)
}

// RefreshConfirmedForSource re-derives strong signals for a source's
// already-confirmed identities from the full archived evidence. It never
// confirms new identities.
func RefreshConfirmedForSource(ctx context.Context, st DiscoveryStore, sourceID int64) error {
	if sourceID <= 0 {
		return opserr.Invalid(errors.New("source ID must be positive"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	existing, err := listAccountIdentities(st, sourceID)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}

	accumulators := make(map[string]*candidateAccumulator)
	rejected := make(map[RejectedCandidate]struct{})
	if _, err := scanIdentityObservationPages(
		ctx, st, sourceID, defaultDiscoveryPageSize, accumulators, rejected, nil,
	); err != nil {
		return err
	}

	_, err = mergeConfirmedIdentitySignals(ctx, st, sourceID, existing, accumulators, rejected)
	return err
}

// scanIdentityObservationPages walks one source's messages in keyset pages,
// merging each page's envelope observations into accumulators. onPage, when
// set, reports cumulative progress and may abort the scan. It returns the
// number of messages scanned.
func scanIdentityObservationPages(
	ctx context.Context,
	st DiscoveryStore,
	sourceID int64,
	pageSize int,
	accumulators map[string]*candidateAccumulator,
	rejected map[RejectedCandidate]struct{},
	onPage func(done int64, candidates int) error,
) (int64, error) {
	var done, afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		page, scanErr := st.ScanIdentityDiscoveryPageContext(ctx, sourceID, afterID, pageSize)
		if scanErr != nil {
			return done, opserr.Internal(fmt.Errorf("scan identity discovery page: %w", scanErr))
		}
		if page.Scanned == 0 {
			return done, nil
		}
		if page.NextAfterID <= afterID {
			return done, opserr.Internal(errors.New("identity discovery page did not advance"))
		}
		mergeIdentityObservations(accumulators, rejected, page.Observations)
		done += page.Scanned
		afterID = page.NextAfterID
		if onPage != nil {
			if err := onPage(done, len(accumulators)); err != nil {
				return done, err
			}
		}
	}
}

// mergeConfirmedIdentitySignals writes only the new strong signals belonging to
// identities the source has already confirmed. Unconfirmed candidates are
// classified for their signal contribution but never reach the write path.
// Filtering here is an optimization; the merge-only store call is the boundary
// that makes confirming a new identity impossible, including when the confirmed
// set went stale during a long scan.
func mergeConfirmedIdentitySignals(
	ctx context.Context,
	st DiscoveryStore,
	sourceID int64,
	existing []store.AccountIdentity,
	accumulators map[string]*candidateAccumulator,
	rejected map[RejectedCandidate]struct{},
) ([]store.IdentityConfirmationOutcome, error) {
	classified := classifyDiscovery(accumulators, rejected, existing)
	confirmed := make(map[string]struct{}, len(classified.candidates))
	for _, candidate := range classified.candidates {
		if candidate.AlreadyConfirmed {
			confirmed[candidate.NormalizedIdentifier] = struct{}{}
		}
	}
	confirmations := make([]store.IdentityConfirmation, 0, len(confirmed))
	for _, confirmation := range classified.confirmations {
		if _, ok := confirmed[store.NormalizeIdentifierForCompare(confirmation.Identifier)]; ok {
			confirmations = append(confirmations, confirmation)
		}
	}
	if len(confirmations) == 0 {
		return []store.IdentityConfirmationOutcome{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outcomes, err := st.MergeConfirmedAccountIdentitySignalsContext(ctx, sourceID, confirmations)
	if err != nil {
		return outcomes, err
	}
	return outcomes, nil
}

// accountIdentityLister is the confirmed-identity read shared by the discovery
// and external-evidence entry points.
type accountIdentityLister interface {
	ListAccountIdentities(sourceID int64) ([]store.AccountIdentity, error)
}

func listAccountIdentities(st accountIdentityLister, sourceID int64) ([]store.AccountIdentity, error) {
	existing, err := st.ListAccountIdentities(sourceID)
	if err != nil {
		return nil, opserr.Internal(fmt.Errorf("list account identities: %w", err))
	}
	return existing, nil
}

func discoveryPageSize(requested int) (int, error) {
	if requested < 0 {
		return 0, opserr.Invalid(errors.New("identity discovery page size must not be negative"))
	}
	if requested == 0 {
		return defaultDiscoveryPageSize, nil
	}
	return min(requested, maxDiscoveryPageSize), nil
}

func mergeIdentityObservations(
	accumulators map[string]*candidateAccumulator,
	rejected map[RejectedCandidate]struct{},
	observations []store.IdentityObservation,
) {
	fromCounts := make(map[int64]int)
	for _, observation := range observations {
		if observation.RecipientType == "from" {
			fromCounts[observation.MessageID]++
		}
	}
	for _, observation := range observations {
		identifier, reason := validateDiscoveredIdentifier(observation.Identifier)
		if reason != "" {
			rejected[RejectedCandidate{Identifier: strings.TrimSpace(observation.Identifier), Reason: reason}] = struct{}{}
			continue
		}
		normalized := store.NormalizeIdentifierForCompare(identifier)
		candidate, ok := accumulators[normalized]
		if !ok {
			candidate = &candidateAccumulator{
				identifier:       identifier,
				normalized:       normalized,
				signals:          make(map[string]struct{}),
				strongSignals:    make(map[string]struct{}),
				existingSignals:  make(map[string]struct{}),
				sentMessages:     make(map[int64]struct{}),
				receivedMessages: make(map[int64]struct{}),
			}
			accumulators[normalized] = candidate
		}
		candidate.observe(observation, fromCounts[observation.MessageID])
	}
}

func validateDiscoveredIdentifier(input string) (string, string) {
	if strings.IndexFunc(input, unicode.IsControl) >= 0 {
		return "", "identifier contains control characters"
	}
	identifier := strings.TrimSpace(input)
	if identifier == "" {
		return "", "identifier is empty"
	}
	if strings.Contains(identifier, "*") {
		return "", "identifier is not a concrete mailbox address"
	}
	parsed, err := mail.ParseAddress(identifier)
	if err != nil || !strings.EqualFold(parsed.Address, identifier) {
		return "", "identifier is not a concrete mailbox address"
	}
	at := strings.LastIndex(identifier, "@")
	if at <= 0 || at == len(identifier)-1 || !strings.Contains(identifier[at+1:], ".") {
		return "", "identifier is not a concrete mailbox address"
	}
	return identifier, ""
}

func (candidate *candidateAccumulator) observe(observation store.IdentityObservation, fromCount int) {
	if !observation.ObservedAt.IsZero() {
		observedAt := observation.ObservedAt.UTC()
		if candidate.firstSeenAt.IsZero() || observedAt.Before(candidate.firstSeenAt) {
			candidate.firstSeenAt = observedAt
		}
		if candidate.lastSeenAt.IsZero() || observedAt.After(candidate.lastSeenAt) {
			candidate.lastSeenAt = observedAt
		}
	}
	if observation.RecipientType != "from" {
		candidate.receivedMessages[observation.MessageID] = struct{}{}
		return
	}
	candidate.sentMessages[observation.MessageID] = struct{}{}
	// Sent placement and is_from_me describe the message, not an individual
	// author. With multiple From addresses, none is attributable without
	// separate address-specific evidence such as a provider alias inventory.
	if fromCount != 1 {
		return
	}
	if observation.IsFromMe {
		candidate.addStrongSignal(SignalIsFromMe)
	}
	if observation.HasSentFolder {
		candidate.addStrongSignal(SignalSentFolder)
	}
	if observation.HasSentLabel {
		candidate.addStrongSignal(SignalSentLabel)
	}
}

func (candidate *candidateAccumulator) addStrongSignal(signal string) {
	candidate.signals[signal] = struct{}{}
	candidate.strongSignals[signal] = struct{}{}
}

func classifyDiscovery(
	accumulators map[string]*candidateAccumulator,
	rejectedSet map[RejectedCandidate]struct{},
	existing []store.AccountIdentity,
) classifiedDiscovery {
	for _, identity := range existing {
		normalized := store.NormalizeIdentifierForCompare(strings.TrimSpace(identity.Address))
		candidate, ok := accumulators[normalized]
		if !ok {
			continue
		}
		candidate.alreadyConfirmed = true
		for _, signal := range SplitSignalSet(identity.SourceSignal) {
			candidate.signals[signal] = struct{}{}
			candidate.existingSignals[signal] = struct{}{}
		}
	}

	classified := classifiedDiscovery{
		candidates:    make([]Candidate, 0, len(accumulators)),
		rejected:      make([]RejectedCandidate, 0, len(rejectedSet)),
		confirmations: make([]store.IdentityConfirmation, 0, len(accumulators)),
	}
	for _, candidate := range accumulators {
		signals := setKeys(candidate.signals)
		classification := classificationWeak
		switch {
		case candidate.alreadyConfirmed:
			classification = classificationConfirmed
		case len(candidate.strongSignals) > 0:
			classification = classificationStrong
		}
		classified.candidates = append(classified.candidates, Candidate{
			Identifier:           candidate.identifier,
			NormalizedIdentifier: candidate.normalized,
			Classification:       classification,
			AlreadyConfirmed:     candidate.alreadyConfirmed,
			Signals:              signals,
			ProviderStates:       []string{},
			SentMessageCount:     int64(len(candidate.sentMessages)),
			ReceivedMessageCount: int64(len(candidate.receivedMessages)),
			FirstSeenAt:          candidate.firstSeenAt,
			LastSeenAt:           candidate.lastSeenAt,
			strongSignals:        setKeys(candidate.strongSignals),
		})

		newStrongSignals := make([]string, 0, len(candidate.strongSignals))
		for signal := range candidate.strongSignals {
			if _, exists := candidate.existingSignals[signal]; !exists {
				newStrongSignals = append(newStrongSignals, signal)
			}
		}
		slices.Sort(newStrongSignals)
		if len(newStrongSignals) > 0 {
			classified.confirmations = append(classified.confirmations, store.IdentityConfirmation{
				Identifier: candidate.identifier,
				Signals:    newStrongSignals,
			})
		}
	}
	for rejected := range rejectedSet {
		classified.rejected = append(classified.rejected, rejected)
	}
	sort.Slice(classified.candidates, func(i, j int) bool {
		left, right := classified.candidates[i], classified.candidates[j]
		if left.NormalizedIdentifier == right.NormalizedIdentifier {
			return left.Identifier < right.Identifier
		}
		return left.NormalizedIdentifier < right.NormalizedIdentifier
	})
	sort.Slice(classified.rejected, func(i, j int) bool {
		if classified.rejected[i].Identifier == classified.rejected[j].Identifier {
			return classified.rejected[i].Reason < classified.rejected[j].Reason
		}
		return classified.rejected[i].Identifier < classified.rejected[j].Identifier
	})
	sort.Slice(classified.confirmations, func(i, j int) bool {
		left := store.NormalizeIdentifierForCompare(classified.confirmations[i].Identifier)
		right := store.NormalizeIdentifierForCompare(classified.confirmations[j].Identifier)
		if left == right {
			return classified.confirmations[i].Identifier < classified.confirmations[j].Identifier
		}
		return left < right
	})
	return classified
}

func strongConfirmations(
	candidates []Candidate,
	existing []store.AccountIdentity,
) []store.IdentityConfirmation {
	existingSignals := make(map[string]map[string]struct{}, len(existing))
	for _, identity := range existing {
		normalized := store.NormalizeIdentifierForCompare(strings.TrimSpace(identity.Address))
		set := existingSignals[normalized]
		if set == nil {
			set = make(map[string]struct{})
			existingSignals[normalized] = set
		}
		for _, signal := range SplitSignalSet(identity.SourceSignal) {
			set[signal] = struct{}{}
		}
	}

	confirmations := make([]store.IdentityConfirmation, 0, len(candidates))
	for _, candidate := range candidates {
		known := existingSignals[candidate.NormalizedIdentifier]
		newSignals := make([]string, 0, len(candidate.strongSignals))
		for _, signal := range candidate.strongSignals {
			if _, exists := known[signal]; !exists {
				newSignals = append(newSignals, signal)
			}
		}
		if len(newSignals) == 0 {
			continue
		}
		confirmations = append(confirmations, store.IdentityConfirmation{
			Identifier: candidate.Identifier,
			Signals:    sortedUnique(newSignals),
		})
	}
	return confirmations
}

func addExplicitWeakConfirmations(classified *classifiedDiscovery, requested []string) error {
	weakByNormalized := make(map[string]Candidate)
	for _, candidate := range classified.candidates {
		if candidate.Classification == classificationWeak {
			weakByNormalized[candidate.NormalizedIdentifier] = candidate
		}
	}
	manual := make(map[string]store.IdentityConfirmation)
	for _, input := range requested {
		identifier := strings.TrimSpace(input)
		normalized := store.NormalizeIdentifierForCompare(identifier)
		candidate, ok := weakByNormalized[normalized]
		if !ok {
			return opserr.Invalid(fmt.Errorf("explicit confirmation %q does not match a weak candidate from the completed scan", identifier))
		}
		manual[normalized] = store.IdentityConfirmation{
			Identifier: candidate.Identifier,
			Signals:    []string{"manual"},
		}
	}
	for _, confirmation := range manual {
		classified.confirmations = append(classified.confirmations, confirmation)
	}
	sort.Slice(classified.confirmations, func(i, j int) bool {
		left := store.NormalizeIdentifierForCompare(classified.confirmations[i].Identifier)
		right := store.NormalizeIdentifierForCompare(classified.confirmations[j].Identifier)
		if left == right {
			return classified.confirmations[i].Identifier < classified.confirmations[j].Identifier
		}
		return left < right
	})
	return nil
}

func setKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func sortedUnique(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return setKeys(set)
}
