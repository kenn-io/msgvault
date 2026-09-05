// Package operations defines the normalized, privacy-bounded read model used
// to project durable subsystem run ledgers. It deliberately owns no storage,
// transport, scheduling, or execution behavior.
package operations

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

const (
	MaxTextStableIDBytes       = 128
	MaxPublicErrorMessageBytes = 256
)

type Kind string

const (
	KindSourceSync         Kind = "source_sync"
	KindPersonSweep        Kind = "person_sweep"
	KindCardDAVSync        Kind = "carddav_sync"
	KindMessageEmbedding   Kind = "message_embedding"
	KindPersonEmbedding    Kind = "person_embedding"
	KindDocumentExtraction Kind = "document_extraction"
	KindDocumentEmbedding  Kind = "document_embedding"
	KindVisualEmbedding    Kind = "visual_embedding"
	KindPersonEnrichment   Kind = "person_enrichment"
)

func (k Kind) Validate() error {
	switch k {
	case KindSourceSync, KindPersonSweep, KindCardDAVSync, KindMessageEmbedding,
		KindPersonEmbedding, KindDocumentExtraction, KindDocumentEmbedding,
		KindVisualEmbedding, KindPersonEnrichment:
		return nil
	default:
		return fmt.Errorf("invalid operation kind %q", k)
	}
}

type Lane string

const (
	LaneMessages          Lane = "messages"
	LanePersonFacts       Lane = "person_facts"
	LaneContacts          Lane = "contacts"
	LaneDocuments         Lane = "documents"
	LaneVisualAttachments Lane = "visual_attachments"
)

func (l Lane) Validate() error {
	switch l {
	case LaneMessages, LanePersonFacts, LaneContacts, LaneDocuments, LaneVisualAttachments:
		return nil
	default:
		return fmt.Errorf("invalid operation lane %q", l)
	}
}

type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StatePartial   State = "partial"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

func (s State) Validate() error {
	switch s {
	case StateQueued, StateRunning, StateSucceeded, StatePartial, StateFailed, StateCancelled:
		return nil
	default:
		return fmt.Errorf("invalid operation state %q", s)
	}
}

type Trigger string

const (
	TriggerManual    Trigger = "manual"
	TriggerScheduled Trigger = "scheduled"
)

func (t Trigger) Validate() error {
	switch t {
	case TriggerManual, TriggerScheduled:
		return nil
	default:
		return fmt.Errorf("invalid operation trigger %q", t)
	}
}

type HistoryAvailability string

const (
	HistoryAvailable   HistoryAvailability = "available"
	HistoryUnavailable HistoryAvailability = "unavailable"
)

func (a HistoryAvailability) Validate() error {
	switch a {
	case HistoryAvailable, HistoryUnavailable:
		return nil
	default:
		return fmt.Errorf("invalid operation history availability %q", a)
	}
}

type ActionID string

const (
	ActionCardDAVSync  ActionID = "carddav_sync"
	ActionVisualBuild  ActionID = "visual_build"
	ActionVisualResume ActionID = "visual_resume"
)

func (a ActionID) Validate() error {
	switch a {
	case ActionCardDAVSync, ActionVisualBuild, ActionVisualResume:
		return nil
	default:
		return fmt.Errorf("invalid operation action %q", a)
	}
}

type RelatedStatusID string

const (
	RelatedStatusSource         RelatedStatusID = "listSourceStatus"
	RelatedStatusDocumentIndex  RelatedStatusID = "getDocumentIndexStatus"
	RelatedStatusDocumentVector RelatedStatusID = "getDocumentVectorStatus"
	RelatedStatusVisual         RelatedStatusID = "getVisualAttachmentStatus"
	RelatedStatusCardDAV        RelatedStatusID = "getCardDAVStatus"
)

func (r RelatedStatusID) Validate() error {
	switch r {
	case RelatedStatusSource, RelatedStatusDocumentIndex, RelatedStatusDocumentVector,
		RelatedStatusVisual, RelatedStatusCardDAV:
		return nil
	default:
		return fmt.Errorf("invalid operation related status %q", r)
	}
}

type LaneDefinition struct {
	Kind                Kind
	Lane                Lane
	HistoryAvailability HistoryAvailability
	UnavailableCode     string
}

var laneRegistry = []LaneDefinition{
	{Kind: KindCardDAVSync, Lane: LaneContacts, HistoryAvailability: HistoryAvailable},
	{Kind: KindDocumentEmbedding, Lane: LaneDocuments, HistoryAvailability: HistoryUnavailable, UnavailableCode: "document_embedding_history_unavailable"},
	{Kind: KindDocumentExtraction, Lane: LaneDocuments, HistoryAvailability: HistoryUnavailable, UnavailableCode: "document_extraction_history_unavailable"},
	{Kind: KindMessageEmbedding, Lane: LaneMessages, HistoryAvailability: HistoryUnavailable, UnavailableCode: "message_embedding_history_unavailable"},
	{Kind: KindPersonEmbedding, Lane: LanePersonFacts, HistoryAvailability: HistoryUnavailable, UnavailableCode: "person_embedding_history_unavailable"},
	{Kind: KindPersonEnrichment, Lane: LanePersonFacts, HistoryAvailability: HistoryUnavailable, UnavailableCode: "person_enrichment_history_unavailable"},
	{Kind: KindPersonSweep, Lane: LanePersonFacts, HistoryAvailability: HistoryAvailable},
	{Kind: KindSourceSync, Lane: LaneMessages, HistoryAvailability: HistoryAvailable},
	{Kind: KindVisualEmbedding, Lane: LaneVisualAttachments, HistoryAvailability: HistoryUnavailable, UnavailableCode: "visual_embedding_history_unavailable"},
}

func LaneRegistry() []LaneDefinition {
	return slices.Clone(laneRegistry)
}

func laneDefinition(kind Kind) (LaneDefinition, bool) {
	index, found := slices.BinarySearchFunc(laneRegistry, kind, func(definition LaneDefinition, target Kind) int {
		return cmp.Compare(definition.Kind, target)
	})
	if !found {
		return LaneDefinition{}, false
	}
	return laneRegistry[index], true
}

type StableIDType string

const (
	StableIDInt64 StableIDType = "int64"
	StableIDText  StableIDType = "text"
)

func (t StableIDType) Validate() error {
	switch t {
	case StableIDInt64, StableIDText:
		return nil
	default:
		return fmt.Errorf("invalid operation stable ID type %q", t)
	}
}

type StableID struct {
	kind    Kind
	idType  StableIDType
	int64ID int64
	textID  string
}

func NewInt64ID(kind Kind, id int64) (StableID, error) {
	stableID := StableID{kind: kind, idType: StableIDInt64, int64ID: id}
	if err := stableID.Validate(); err != nil {
		return StableID{}, err
	}
	return stableID, nil
}

func NewTextID(kind Kind, id string) (StableID, error) {
	stableID := StableID{kind: kind, idType: StableIDText, textID: id}
	if err := stableID.Validate(); err != nil {
		return StableID{}, err
	}
	return stableID, nil
}

func (id StableID) Kind() Kind { return id.kind }

func (id StableID) Type() StableIDType { return id.idType }

func (id StableID) Int64() (int64, bool) {
	return id.int64ID, id.idType == StableIDInt64
}

func (id StableID) Text() (string, bool) {
	return id.textID, id.idType == StableIDText
}

func (id StableID) Validate() error {
	if err := id.kind.Validate(); err != nil {
		return err
	}
	wantType, ok := stableIDTypeForKind(id.kind)
	if !ok {
		return fmt.Errorf("operation kind %q has no durable history ID", id.kind)
	}
	if id.idType != wantType {
		return fmt.Errorf("operation kind %q requires %q stable IDs", id.kind, wantType)
	}
	switch id.idType {
	case StableIDInt64:
		if id.int64ID <= 0 || id.textID != "" {
			return errors.New("operation numeric stable ID must be positive and exclusively numeric")
		}
	case StableIDText:
		if id.int64ID != 0 || id.textID == "" || strings.TrimSpace(id.textID) != id.textID ||
			!utf8.ValidString(id.textID) || len(id.textID) > MaxTextStableIDBytes {
			return errors.New("operation text stable ID must be canonical, valid UTF-8, and bounded")
		}
	default:
		return fmt.Errorf("invalid operation stable ID type %q", id.idType)
	}
	return nil
}

func stableIDTypeForKind(kind Kind) (StableIDType, bool) {
	switch kind {
	case KindSourceSync, KindCardDAVSync, KindMessageEmbedding, KindPersonEmbedding,
		KindDocumentExtraction, KindDocumentEmbedding, KindVisualEmbedding, KindPersonEnrichment:
		return StableIDInt64, true
	case KindPersonSweep:
		return StableIDText, true
	default:
		return "", false
	}
}

type CounterName string

const (
	CounterProcessed        CounterName = "processed"
	CounterAdded            CounterName = "added"
	CounterUpdated          CounterName = "updated"
	CounterItemErrors       CounterName = "item_errors"
	CounterAttempted        CounterName = "attempted"
	CounterSucceeded        CounterName = "succeeded"
	CounterFailed           CounterName = "failed"
	CounterProjectedWrites  CounterName = "projected_writes"
	CounterBooks            CounterName = "books"
	CounterCreated          CounterName = "created"
	CounterRemoved          CounterName = "removed"
	CounterTruncated        CounterName = "truncated"
	CounterSkipped          CounterName = "skipped"
	CounterRequested        CounterName = "requested"
	CounterStarted          CounterName = "started"
	CounterSuppressed       CounterName = "suppressed"
	CounterIdentityRejected CounterName = "identity_rejected"
)

func (n CounterName) Validate() error {
	switch n {
	case CounterProcessed, CounterAdded, CounterUpdated, CounterItemErrors,
		CounterAttempted, CounterSucceeded, CounterFailed, CounterProjectedWrites,
		CounterBooks, CounterCreated, CounterRemoved, CounterTruncated, CounterSkipped,
		CounterRequested, CounterStarted, CounterSuppressed, CounterIdentityRejected:
		return nil
	default:
		return fmt.Errorf("invalid operation counter name %q", n)
	}
}

// CounterNames returns every counter name the closed operation registry can emit.
func CounterNames() []CounterName {
	set := make(map[CounterName]struct{})
	for _, counters := range counterUnitsByKind {
		for name := range counters {
			set[name] = struct{}{}
		}
	}
	result := make([]CounterName, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

type CounterUnit string

const (
	CounterUnitMessages    CounterUnit = "messages"
	CounterUnitPeople      CounterUnit = "people"
	CounterUnitWrites      CounterUnit = "writes"
	CounterUnitBooks       CounterUnit = "books"
	CounterUnitContacts    CounterUnit = "contacts"
	CounterUnitDocuments   CounterUnit = "documents"
	CounterUnitChunks      CounterUnit = "chunks"
	CounterUnitAttachments CounterUnit = "attachments"
)

func (u CounterUnit) Validate() error {
	switch u {
	case CounterUnitMessages, CounterUnitPeople, CounterUnitWrites, CounterUnitBooks, CounterUnitContacts,
		CounterUnitDocuments, CounterUnitChunks, CounterUnitAttachments:
		return nil
	default:
		return fmt.Errorf("invalid operation counter unit %q", u)
	}
}

// CounterUnits returns every counter unit the closed operation registry can emit.
func CounterUnits() []CounterUnit {
	set := make(map[CounterUnit]struct{})
	for _, counters := range counterUnitsByKind {
		for _, unit := range counters {
			set[unit] = struct{}{}
		}
	}
	result := make([]CounterUnit, 0, len(set))
	for unit := range set {
		result = append(result, unit)
	}
	slices.Sort(result)
	return result
}

type PublicCounter struct {
	Name  CounterName
	Unit  CounterUnit
	Value int64
}

var counterUnitsByKind = map[Kind]map[CounterName]CounterUnit{
	KindSourceSync: {
		CounterProcessed:  CounterUnitMessages,
		CounterAdded:      CounterUnitMessages,
		CounterUpdated:    CounterUnitMessages,
		CounterItemErrors: CounterUnitMessages,
	},
	KindPersonSweep: {
		CounterAttempted:       CounterUnitPeople,
		CounterSucceeded:       CounterUnitPeople,
		CounterFailed:          CounterUnitPeople,
		CounterProjectedWrites: CounterUnitWrites,
	},
	KindCardDAVSync: {
		CounterBooks:   CounterUnitBooks,
		CounterCreated: CounterUnitContacts,
		CounterUpdated: CounterUnitContacts,
		CounterRemoved: CounterUnitContacts,
	},
	KindMessageEmbedding: {
		CounterAttempted: CounterUnitMessages,
		CounterSucceeded: CounterUnitMessages,
		CounterFailed:    CounterUnitMessages,
		CounterTruncated: CounterUnitMessages,
	},
	KindPersonEmbedding: {
		CounterAttempted: CounterUnitPeople,
		CounterSucceeded: CounterUnitPeople,
		CounterFailed:    CounterUnitPeople,
		CounterTruncated: CounterUnitPeople,
	},
	KindDocumentExtraction: {
		CounterAttempted: CounterUnitDocuments,
		CounterSucceeded: CounterUnitDocuments,
		CounterFailed:    CounterUnitDocuments,
	},
	KindDocumentEmbedding: {
		CounterAttempted: CounterUnitChunks,
		CounterSucceeded: CounterUnitChunks,
		CounterFailed:    CounterUnitChunks,
	},
	KindVisualEmbedding: {
		CounterAttempted: CounterUnitAttachments,
		CounterSucceeded: CounterUnitAttachments,
		CounterFailed:    CounterUnitAttachments,
		CounterSkipped:   CounterUnitAttachments,
	},
	KindPersonEnrichment: {
		CounterRequested:        CounterUnitPeople,
		CounterStarted:          CounterUnitPeople,
		CounterSucceeded:        CounterUnitPeople,
		CounterFailed:           CounterUnitPeople,
		CounterSuppressed:       CounterUnitPeople,
		CounterIdentityRejected: CounterUnitPeople,
	},
}

func ValidateCounters(kind Kind, counters []PublicCounter) error {
	if err := kind.Validate(); err != nil {
		return err
	}
	allowed, ok := counterUnitsByKind[kind]
	if !ok {
		return fmt.Errorf("operation kind %q has no durable public counters", kind)
	}
	seen := make(map[CounterName]struct{}, len(counters))
	for _, counter := range counters {
		if err := counter.Name.Validate(); err != nil {
			return err
		}
		if counter.Value < 0 {
			return fmt.Errorf("operation counter %q must be nonnegative", counter.Name)
		}
		wantUnit, ok := allowed[counter.Name]
		if !ok {
			return fmt.Errorf("operation counter %q is not allowed for kind %q", counter.Name, kind)
		}
		if counter.Unit != wantUnit {
			return fmt.Errorf("operation counter %q requires unit %q", counter.Name, wantUnit)
		}
		if _, duplicate := seen[counter.Name]; duplicate {
			return fmt.Errorf("duplicate operation counter %q", counter.Name)
		}
		seen[counter.Name] = struct{}{}
	}
	return nil
}

type PublicErrorCode string

const (
	PublicErrorSourceSyncFailed               PublicErrorCode = "source_sync_failed"
	PublicErrorPersonSweepFailed              PublicErrorCode = "person_sweep_failed"
	PublicErrorPolicy                         PublicErrorCode = "policy"
	PublicErrorBudget                         PublicErrorCode = "budget"
	PublicErrorLeaseLost                      PublicErrorCode = "lease_lost"
	PublicErrorRateLimited                    PublicErrorCode = "rate_limited"
	PublicErrorTimeout                        PublicErrorCode = "timeout"
	PublicErrorProviderHTTP                   PublicErrorCode = "provider_http"
	PublicErrorInvalidOutput                  PublicErrorCode = "invalid_output"
	PublicErrorArchiveGap                     PublicErrorCode = "archive_gap"
	PublicErrorInternal                       PublicErrorCode = "internal"
	PublicErrorCancelled                      PublicErrorCode = "cancelled"
	PublicErrorRetryAfter                     PublicErrorCode = "retry_after"
	PublicErrorAuthenticationFailed           PublicErrorCode = "authentication_failed"
	PublicErrorUpstreamFailed                 PublicErrorCode = "upstream_failed"
	PublicErrorSafetyLimit                    PublicErrorCode = "safety_limit"
	PublicErrorSyncFailed                     PublicErrorCode = "sync_failed"
	PublicErrorUnsafeErrorRedacted            PublicErrorCode = "unsafe_error_redacted"
	PublicErrorDaemonRestarted                PublicErrorCode = "daemon_restarted"
	PublicErrorCardDAVSyncFailed              PublicErrorCode = "carddav_sync_failed"
	PublicErrorInvocationCancelled            PublicErrorCode = "invocation_cancelled"
	PublicErrorInvocationTimeout              PublicErrorCode = "invocation_timeout"
	PublicErrorInvocationRateLimited          PublicErrorCode = "invocation_rate_limited"
	PublicErrorInvocationAuthenticationFailed PublicErrorCode = "invocation_authentication_failed"
	PublicErrorInvocationUpstreamFailed       PublicErrorCode = "invocation_upstream_failed"
	PublicErrorInvocationInvalidOutput        PublicErrorCode = "invocation_invalid_output"
	PublicErrorInvocationSafetyLimit          PublicErrorCode = "invocation_safety_limit"
	PublicErrorInvocationArchiveDrift         PublicErrorCode = "invocation_archive_drift"
	PublicErrorInvocationDaemonRestarted      PublicErrorCode = "invocation_daemon_restarted"
	PublicErrorInvocationInternal             PublicErrorCode = "invocation_internal"
	PublicErrorInvocationUnsafeErrorRedacted  PublicErrorCode = "invocation_unsafe_error_redacted"
)

func (c PublicErrorCode) Validate() error {
	if _, ok := fixedPublicErrorMessages[c]; !ok {
		return fmt.Errorf("invalid operation public error code %q", c)
	}
	return nil
}

type PublicError struct {
	Code    PublicErrorCode
	Message string
}

var fixedPublicErrorMessages = map[PublicErrorCode]string{
	PublicErrorSourceSyncFailed:               "Source sync failed.",
	PublicErrorPersonSweepFailed:              "Person sweep failed.",
	PublicErrorPolicy:                         "Person sweep was blocked by policy.",
	PublicErrorBudget:                         "Person sweep budget was exhausted.",
	PublicErrorLeaseLost:                      "Person sweep ownership expired.",
	PublicErrorRateLimited:                    "Person sweep was rate limited.",
	PublicErrorTimeout:                        "Person sweep timed out.",
	PublicErrorProviderHTTP:                   "Person sweep provider request failed.",
	PublicErrorInvalidOutput:                  "Person sweep provider output was invalid.",
	PublicErrorArchiveGap:                     "Person sweep archive input changed.",
	PublicErrorInternal:                       "Person sweep failed internally.",
	PublicErrorCancelled:                      "CardDAV sync was cancelled.",
	PublicErrorRetryAfter:                     "CardDAV sync is temporarily paused.",
	PublicErrorAuthenticationFailed:           "CardDAV authentication failed.",
	PublicErrorUpstreamFailed:                 "CardDAV server request failed.",
	PublicErrorSafetyLimit:                    "CardDAV sync exceeded its safety limits.",
	PublicErrorSyncFailed:                     "CardDAV sync failed.",
	PublicErrorUnsafeErrorRedacted:            "CardDAV sync failed; sensitive details were removed.",
	PublicErrorDaemonRestarted:                "CardDAV sync stopped because the daemon restarted.",
	PublicErrorCardDAVSyncFailed:              "CardDAV sync failed.",
	PublicErrorInvocationCancelled:            "Operation was cancelled.",
	PublicErrorInvocationTimeout:              "Operation timed out.",
	PublicErrorInvocationRateLimited:          "Operation was rate limited.",
	PublicErrorInvocationAuthenticationFailed: "Operation authentication failed.",
	PublicErrorInvocationUpstreamFailed:       "Upstream operation failed.",
	PublicErrorInvocationInvalidOutput:        "Operation output was invalid.",
	PublicErrorInvocationSafetyLimit:          "Operation exceeded its safety limits.",
	PublicErrorInvocationArchiveDrift:         "Operation archive input changed.",
	PublicErrorInvocationDaemonRestarted:      "Operation stopped because the daemon restarted.",
	PublicErrorInvocationInternal:             "Operation failed internally.",
	PublicErrorInvocationUnsafeErrorRedacted:  "Operation failed; sensitive details were removed.",
}

// PublicErrorCodes returns every fixed public error code the runtime can emit.
func PublicErrorCodes() []PublicErrorCode {
	result := make([]PublicErrorCode, 0, len(fixedPublicErrorMessages))
	for code := range fixedPublicErrorMessages {
		result = append(result, code)
	}
	slices.Sort(result)
	return result
}

// ValidateInvocationPublicError closes recorder errors independently from
// subsystem-specific public errors that happen to share similar categories.
func ValidateInvocationPublicError(kind Kind, publicError *PublicError) error {
	if publicError == nil {
		return nil
	}
	if err := publicError.Validate(); err != nil {
		return err
	}
	switch kind {
	case KindMessageEmbedding, KindPersonEmbedding, KindDocumentExtraction,
		KindDocumentEmbedding, KindVisualEmbedding, KindPersonEnrichment:
	default:
		return fmt.Errorf("operation kind %q has no invocation public errors", kind)
	}
	switch publicError.Code {
	case PublicErrorInvocationCancelled, PublicErrorInvocationTimeout,
		PublicErrorInvocationRateLimited, PublicErrorInvocationAuthenticationFailed,
		PublicErrorInvocationUpstreamFailed, PublicErrorInvocationInvalidOutput,
		PublicErrorInvocationSafetyLimit, PublicErrorInvocationArchiveDrift,
		PublicErrorInvocationDaemonRestarted, PublicErrorInvocationInternal,
		PublicErrorInvocationUnsafeErrorRedacted:
		return nil
	default:
		return fmt.Errorf("operation kind %q cannot use public error code %q", kind, publicError.Code)
	}
}

func (e PublicError) Validate() error {
	if err := e.Code.Validate(); err != nil {
		return err
	}
	want := fixedPublicErrorMessages[e.Code]
	if e.Message != want {
		return fmt.Errorf("operation public error %q must use its fixed message", e.Code)
	}
	if e.Message == "" || !utf8.ValidString(e.Message) || len(e.Message) > MaxPublicErrorMessageBytes {
		return errors.New("operation public error message must be nonempty, valid UTF-8, and bounded")
	}
	return nil
}

func newPublicError(code PublicErrorCode) *PublicError {
	return &PublicError{Code: code, Message: fixedPublicErrorMessages[code]}
}

// FixedPublicError returns the only public-safe error for code. Callers cannot
// attach provider or content details to the returned error.
func FixedPublicError(code PublicErrorCode) *PublicError {
	if code.Validate() != nil {
		return nil
	}
	return newPublicError(code)
}

func ProjectSourceState(
	durableState string,
	itemErrors, messagesAdded, messagesUpdated int64,
) (State, *PublicError, error) {
	if itemErrors < 0 || messagesAdded < 0 || messagesUpdated < 0 {
		return "", nil, errors.New("source sync operation counters must be nonnegative")
	}
	switch durableState {
	case "running":
		return StateRunning, nil, nil
	case "completed":
		if itemErrors > 0 {
			return StatePartial, nil, nil
		}
		return StateSucceeded, nil, nil
	case "failed":
		if messagesAdded > 0 || messagesUpdated > 0 {
			return StatePartial, newPublicError(PublicErrorSourceSyncFailed), nil
		}
		return StateFailed, newPublicError(PublicErrorSourceSyncFailed), nil
	case "cancelled":
		return StateCancelled, nil, nil
	default:
		return "", nil, fmt.Errorf("unknown source sync operation state %q", durableState)
	}
}

func ProjectPersonSweepFailure(class peoplesweep.FailureClass) PublicError {
	code := PublicErrorPersonSweepFailed
	switch class {
	case peoplesweep.FailurePolicy:
		code = PublicErrorPolicy
	case peoplesweep.FailureBudget:
		code = PublicErrorBudget
	case peoplesweep.FailureLeaseLost:
		code = PublicErrorLeaseLost
	case peoplesweep.FailureRateLimited:
		code = PublicErrorRateLimited
	case peoplesweep.FailureTimeout:
		code = PublicErrorTimeout
	case peoplesweep.FailureProviderHTTP:
		code = PublicErrorProviderHTTP
	case peoplesweep.FailureInvalidOutput:
		code = PublicErrorInvalidOutput
	case peoplesweep.FailureArchiveGap:
		code = PublicErrorArchiveGap
	case peoplesweep.FailureInternal:
		code = PublicErrorInternal
	}
	return *newPublicError(code)
}

func ProjectCardDAVFailure(durableCode string) *PublicError {
	var code PublicErrorCode
	switch durableCode {
	case "":
		return nil
	case "cancelled":
		code = PublicErrorCancelled
	case "retry_after":
		code = PublicErrorRetryAfter
	case "authentication_failed":
		code = PublicErrorAuthenticationFailed
	case "upstream_failed":
		code = PublicErrorUpstreamFailed
	case "safety_limit":
		code = PublicErrorSafetyLimit
	case "sync_failed":
		code = PublicErrorSyncFailed
	case "unsafe_error_redacted":
		code = PublicErrorUnsafeErrorRedacted
	case "daemon_restarted":
		code = PublicErrorDaemonRestarted
	default:
		code = PublicErrorCardDAVSyncFailed
	}
	return newPublicError(code)
}

type Run struct {
	ID         StableID
	Lane       Lane
	State      State
	Trigger    *Trigger
	StartedAt  time.Time
	FinishedAt *time.Time
	Counters   []PublicCounter
	Error      *PublicError
}

// TerminalReplayError is the fixed, privacy-safe non-success returned when an
// idempotent invocation reuses a terminal operation run. It never contains the
// stored provider error, request data, or operation identifier.
type TerminalReplayError struct {
	state State
	code  PublicErrorCode
}

func (e *TerminalReplayError) Error() string {
	return fixedPublicErrorMessages[e.code]
}

func (e *TerminalReplayError) State() State { return e.state }

func (e *TerminalReplayError) Code() PublicErrorCode { return e.code }

func (e *TerminalReplayError) Unwrap() error {
	switch e.code {
	case PublicErrorCancelled, PublicErrorInvocationCancelled:
		return context.Canceled
	case PublicErrorTimeout, PublicErrorInvocationTimeout:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// TerminalReplayOutcome reconstructs only the public terminal semantics of a
// durable run. Successful and partial runs remain useful completed outcomes;
// failed and cancelled runs return a typed error containing only fixed text.
func TerminalReplayOutcome(run *Run) error {
	if run == nil {
		return errors.New("operation terminal replay outcome is required")
	}
	if err := run.Validate(); err != nil {
		return errors.New("operation terminal replay outcome is invalid")
	}
	switch run.State {
	case StateSucceeded, StatePartial:
		return nil
	case StateCancelled:
		code := PublicErrorInvocationCancelled
		if run.Error != nil {
			code = run.Error.Code
		}
		return &TerminalReplayError{state: run.State, code: code}
	case StateFailed:
		if run.Error == nil {
			return errors.New("operation terminal replay outcome is invalid")
		}
		return &TerminalReplayError{state: run.State, code: run.Error.Code}
	default:
		return errors.New("operation terminal replay outcome is not terminal")
	}
}

func (r Run) Validate() error {
	if err := r.ID.Validate(); err != nil {
		return err
	}
	definition, ok := laneDefinition(r.ID.Kind())
	if !ok || r.Lane != definition.Lane {
		return fmt.Errorf("operation kind %q cannot use lane %q", r.ID.Kind(), r.Lane)
	}
	if err := r.State.Validate(); err != nil {
		return err
	}
	if r.State == StateQueued && r.ID.Kind() != KindPersonEnrichment {
		return fmt.Errorf("operation kind %q has no durable queued runs", r.ID.Kind())
	}
	if r.Trigger != nil {
		if err := r.Trigger.Validate(); err != nil {
			return err
		}
		if r.ID.Kind() == KindSourceSync {
			return errors.New("source sync history has no truthful trigger")
		}
	}
	if r.StartedAt.IsZero() {
		return errors.New("operation start time is required")
	}
	if r.StartedAt.Location() != time.UTC {
		return errors.New("operation start time must be normalized to UTC")
	}
	if r.FinishedAt != nil && r.FinishedAt.Before(r.StartedAt) {
		return errors.New("operation finish time cannot precede its start")
	}
	if r.FinishedAt != nil && r.FinishedAt.Location() != time.UTC {
		return errors.New("operation finish time must be normalized to UTC")
	}
	if (r.State == StateQueued || r.State == StateRunning) && r.FinishedAt != nil {
		return fmt.Errorf("active operation state %q cannot have a finish time", r.State)
	}
	if r.State != StateQueued && r.State != StateRunning && r.FinishedAt == nil {
		return fmt.Errorf("terminal operation state %q requires a finish time", r.State)
	}
	if err := ValidateCounters(r.ID.Kind(), r.Counters); err != nil {
		return err
	}
	if r.Error != nil {
		if err := r.Error.Validate(); err != nil {
			return err
		}
	}
	return validateRunStateAndError(r.ID.Kind(), r.State, r.Counters, r.Error)
}

func validateRunStateAndError(
	kind Kind,
	state State,
	counters []PublicCounter,
	publicError *PublicError,
) error {
	switch kind {
	case KindSourceSync:
		added := publicCounterValue(counters, CounterAdded)
		updated := publicCounterValue(counters, CounterUpdated)
		itemErrors := publicCounterValue(counters, CounterItemErrors)
		switch state {
		case StateRunning, StateCancelled:
			if publicError != nil {
				return fmt.Errorf("source sync state %q cannot carry a public error", state)
			}
		case StateSucceeded:
			if publicError != nil {
				return fmt.Errorf("source sync state %q cannot carry a public error", state)
			}
			if itemErrors != 0 {
				return errors.New("succeeded source sync cannot have item errors")
			}
		case StatePartial:
			if publicError == nil {
				if itemErrors <= 0 {
					return errors.New("completed-origin partial source sync requires item errors")
				}
				break
			}
			if publicError.Code != PublicErrorSourceSyncFailed {
				return fmt.Errorf("source sync state %q has a cross-kind public error", state)
			}
			if added <= 0 && updated <= 0 {
				return errors.New("failed-origin partial source sync requires an added or updated item")
			}
		case StateFailed:
			if publicError == nil || publicError.Code != PublicErrorSourceSyncFailed {
				return errors.New("failed source sync requires its fixed public error")
			}
			if added != 0 || updated != 0 {
				return errors.New("failed source sync cannot have added or updated items")
			}
		default:
			return fmt.Errorf("source sync does not support operation state %q", state)
		}
	case KindPersonSweep:
		switch state {
		case StateRunning, StateSucceeded:
			if publicError != nil {
				return fmt.Errorf("person sweep state %q cannot carry a public error", state)
			}
		case StatePartial, StateFailed:
			if publicError == nil || !isPersonSweepError(publicError.Code) {
				return fmt.Errorf("person sweep state %q requires a fixed people-sweep public error", state)
			}
		default:
			return fmt.Errorf("person sweep does not support operation state %q", state)
		}
	case KindCardDAVSync:
		switch state {
		case StateRunning, StateSucceeded:
			if publicError != nil {
				return fmt.Errorf("CardDAV sync state %q cannot carry a public error", state)
			}
		case StatePartial, StateFailed, StateCancelled:
			if publicError == nil || !isCardDAVError(publicError.Code) {
				return fmt.Errorf("CardDAV sync state %q requires a fixed CardDAV public error", state)
			}
		default:
			return fmt.Errorf("CardDAV sync does not support operation state %q", state)
		}
	case KindMessageEmbedding, KindPersonEmbedding, KindDocumentExtraction,
		KindDocumentEmbedding, KindVisualEmbedding, KindPersonEnrichment:
		return validateInvocationRunState(kind, state, counters, publicError)
	default:
		return fmt.Errorf("operation kind %q has no durable run validation", kind)
	}
	return nil
}

func validateInvocationRunState(kind Kind, state State, counters []PublicCounter, publicError *PublicError) error {
	if state == StateQueued {
		if kind != KindPersonEnrichment || publicError != nil {
			return fmt.Errorf("operation kind %q cannot use queued state with a public error", kind)
		}
		return nil
	}
	if state == StateRunning {
		if publicError != nil {
			return fmt.Errorf("running operation kind %q cannot carry a public error", kind)
		}
		return nil
	}
	invocationCounters, err := InvocationCountersFromPublic(kind, counters)
	if err != nil {
		return err
	}
	if err := invocationCounters.ValidateFinal(kind); err != nil {
		return err
	}
	derivedState, err := DeriveInvocationState(kind, invocationCounters, publicError)
	if err != nil {
		return err
	}
	if state != derivedState {
		return fmt.Errorf("operation invocation state %q does not match derived state %q", state, derivedState)
	}
	useful := invocationCounters.Succeeded > 0
	hasFailures := invocationCounters.Failed > 0 || publicError != nil
	switch state {
	case StateSucceeded:
		if hasFailures {
			return errors.New("succeeded invocation cannot have failures")
		}
	case StatePartial:
		if !useful || !hasFailures {
			return errors.New("partial invocation requires useful outcomes and failures")
		}
	case StateFailed:
		if useful || !hasFailures {
			return errors.New("failed invocation requires no useful outcomes and at least one failure")
		}
	case StateCancelled:
		if publicError == nil || publicError.Code != PublicErrorInvocationCancelled {
			return errors.New("cancelled invocation requires its fixed public error")
		}
	default:
		return fmt.Errorf("operation kind %q does not support state %q", kind, state)
	}
	return nil
}

func publicCounterValue(counters []PublicCounter, name CounterName) int64 {
	for _, counter := range counters {
		if counter.Name == name {
			return counter.Value
		}
	}
	return 0
}

func isPersonSweepError(code PublicErrorCode) bool {
	switch code {
	case PublicErrorPersonSweepFailed, PublicErrorPolicy, PublicErrorBudget,
		PublicErrorLeaseLost, PublicErrorRateLimited, PublicErrorTimeout,
		PublicErrorProviderHTTP, PublicErrorInvalidOutput, PublicErrorArchiveGap,
		PublicErrorInternal:
		return true
	default:
		return false
	}
}

func isCardDAVError(code PublicErrorCode) bool {
	switch code {
	case PublicErrorCancelled, PublicErrorRetryAfter, PublicErrorAuthenticationFailed,
		PublicErrorUpstreamFailed, PublicErrorSafetyLimit, PublicErrorSyncFailed,
		PublicErrorUnsafeErrorRedacted, PublicErrorDaemonRestarted,
		PublicErrorCardDAVSyncFailed:
		return true
	default:
		return false
	}
}

// CompareRuns returns a negative value when left precedes right in the public
// newest-first ordering, zero when their ordering keys match, and a positive
// value when right precedes left.
func CompareRuns(left, right Run) int {
	if compared := left.StartedAt.UTC().Compare(right.StartedAt.UTC()); compared != 0 {
		return -compared
	}
	if compared := cmp.Compare(left.ID.Kind(), right.ID.Kind()); compared != 0 {
		return compared
	}
	return compareStableIDDescending(left.ID, right.ID)
}

func compareStableIDDescending(left, right StableID) int {
	if compared := cmp.Compare(left.Type(), right.Type()); compared != 0 {
		return compared
	}
	switch left.Type() {
	case StableIDInt64:
		return -cmp.Compare(left.int64ID, right.int64ID)
	case StableIDText:
		return -cmp.Compare(left.textID, right.textID)
	default:
		return 0
	}
}

func SortRuns(runs []Run) {
	slices.SortFunc(runs, CompareRuns)
}

type Position struct {
	StartedAt time.Time
	ID        StableID
}

func (p Position) Validate() error {
	if p.StartedAt.IsZero() {
		return errors.New("operation history position time is required")
	}
	if p.StartedAt.Location() != time.UTC {
		return errors.New("operation history position time must be normalized to UTC")
	}
	return p.ID.Validate()
}

type Query struct {
	Kinds         []Kind
	States        []State
	StartedFrom   *time.Time
	StartedBefore *time.Time
	Position      *Position
	Limit         int
}

func (q Query) Validate() error {
	if q.Limit < 1 || q.Limit > 100 {
		return errors.New("operation history limit must be between 1 and 100")
	}
	for index, kind := range q.Kinds {
		if err := kind.Validate(); err != nil {
			return err
		}
		if index > 0 && q.Kinds[index-1] >= kind {
			return errors.New("operation history kinds must be sorted and unique")
		}
	}
	for index, state := range q.States {
		if err := state.Validate(); err != nil {
			return err
		}
		if index > 0 && q.States[index-1] >= state {
			return errors.New("operation history states must be sorted and unique")
		}
	}
	if q.StartedFrom != nil {
		if q.StartedFrom.IsZero() {
			return errors.New("operation history lower date bound is required when present")
		}
		if q.StartedFrom.Location() != time.UTC {
			return errors.New("operation history lower date bound must be normalized to UTC")
		}
	}
	if q.StartedBefore != nil {
		if q.StartedBefore.IsZero() {
			return errors.New("operation history upper date bound is required when present")
		}
		if q.StartedBefore.Location() != time.UTC {
			return errors.New("operation history upper date bound must be normalized to UTC")
		}
	}
	if q.StartedFrom != nil && q.StartedBefore != nil && !q.StartedFrom.Before(*q.StartedBefore) {
		return errors.New("operation history lower date bound must precede upper date bound")
	}
	if q.Position != nil {
		if err := q.Position.Validate(); err != nil {
			return err
		}
		if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, q.Position.ID.Kind()) {
			return errors.New("operation history position kind is not selected")
		}
	}
	return nil
}

type HistorySnapshot struct {
	Runs               []Run
	Position           *Position
	AvailableKinds     []Kind
	UnavailableKinds   []Kind
	MembershipRevision int64
}

type LaneHistoryStatus struct {
	Kind                Kind
	Lane                Lane
	HistoryAvailability HistoryAvailability
	UnavailableCode     string
	Active              *Run
	Latest              *Run
	LatestSuccessful    *Run
}

func (s LaneHistoryStatus) Validate() error {
	if err := s.Kind.Validate(); err != nil {
		return err
	}
	definition, ok := laneDefinition(s.Kind)
	if !ok {
		return fmt.Errorf("operation kind %q has no lane definition", s.Kind)
	}
	if s.Lane != definition.Lane {
		return fmt.Errorf("operation kind %q requires lane %q", s.Kind, definition.Lane)
	}
	if err := s.HistoryAvailability.Validate(); err != nil {
		return err
	}
	if s.HistoryAvailability == HistoryUnavailable {
		if s.UnavailableCode != string(s.Kind)+"_history_unavailable" {
			return fmt.Errorf("operation kind %q has an invalid history availability code", s.Kind)
		}
		if s.Active != nil || s.Latest != nil || s.LatestSuccessful != nil {
			return fmt.Errorf("unavailable operation history %q cannot contain runs", s.Kind)
		}
		return nil
	}
	if s.UnavailableCode != "" {
		return fmt.Errorf("available operation history %q cannot carry an unavailable code", s.Kind)
	}
	if err := validateStatusRun(s, "active", s.Active, ""); err != nil {
		return err
	}
	if s.Active != nil && s.Active.State != StateRunning &&
		(s.Kind != KindPersonEnrichment || s.Active.State != StateQueued) {
		return errors.New("operation history active run must have an active state")
	}
	if err := validateStatusRun(s, "latest", s.Latest, ""); err != nil {
		return err
	}
	if err := validateStatusRun(s, "latest successful", s.LatestSuccessful, StateSucceeded); err != nil {
		return err
	}
	if s.LatestSuccessful != nil {
		if s.Latest == nil {
			return errors.New("operation history latest successful run requires a latest run")
		}
		if CompareRuns(*s.LatestSuccessful, *s.Latest) < 0 {
			return errors.New("operation history latest successful run cannot be newer than its latest run")
		}
	}
	return nil
}

func validateStatusRun(status LaneHistoryStatus, role string, run *Run, requiredState State) error {
	if run == nil {
		return nil
	}
	if err := run.Validate(); err != nil {
		return fmt.Errorf("validate operation history %s run: %w", role, err)
	}
	if run.ID.Kind() != status.Kind || run.Lane != status.Lane {
		return fmt.Errorf("operation history %s run does not belong to its lane status", role)
	}
	if requiredState != "" && run.State != requiredState {
		return fmt.Errorf("operation history %s run must have state %q", role, requiredState)
	}
	return nil
}

type HistoryReader interface {
	Kinds() []Kind
	ListRuns(ctx context.Context, query Query) (HistorySnapshot, error)
	GetRun(ctx context.Context, id StableID) (Run, error)
	LaneStatus(ctx context.Context, kind Kind) (LaneHistoryStatus, error)
}
