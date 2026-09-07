package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxInvocationKeyBytes = 128

// PassScope is the caller-owned identity for one bounded execution pass.
// Workers may specialize Key by operation kind, but never derive it from
// provider data or archive content.
type PassScope struct {
	Key       string
	Trigger   Trigger
	StartedAt time.Time
}

func (s PassScope) Validate() error {
	return InvocationSpec{
		Kind: KindMessageEmbedding, Key: s.Key, Trigger: s.Trigger, StartedAt: s.StartedAt,
	}.Validate()
}

func (s PassScope) InvocationSpec(kind Kind) InvocationSpec {
	return InvocationSpec{
		Kind: kind, Key: s.Key, Trigger: s.Trigger, StartedAt: s.StartedAt,
	}
}

// ForKind closes a coordinator-owned base scope over one invocation ledger.
// Separate per-kind ledgers could technically share a key, but distinct keys
// keep the private durable identity unambiguous during diagnosis and recovery.
func (s PassScope) ForKind(kind Kind) (PassScope, error) {
	if err := s.Validate(); err != nil {
		return PassScope{}, err
	}
	s.Key += ":" + string(kind)
	if err := s.InvocationSpec(kind).Validate(); err != nil {
		return PassScope{}, err
	}
	return s, nil
}

type BeginDisposition string

const (
	BeginCreated  BeginDisposition = "created"
	BeginActive   BeginDisposition = "active"
	BeginTerminal BeginDisposition = "terminal"
)

type InvocationSpec struct {
	Kind      Kind
	Key       string
	Trigger   Trigger
	StartedAt time.Time
}

func (s InvocationSpec) Validate() error {
	if !isRecordedInvocationKind(s.Kind) {
		return fmt.Errorf("operation kind %q has no invocation recorder", s.Kind)
	}
	if s.Key == "" || strings.TrimSpace(s.Key) != s.Key || !utf8.ValidString(s.Key) || len(s.Key) > MaxInvocationKeyBytes {
		return errors.New("operation invocation key must be canonical, valid UTF-8, and bounded")
	}
	if err := s.Trigger.Validate(); err != nil {
		return err
	}
	if s.StartedAt.IsZero() {
		return errors.New("operation invocation start time is required")
	}
	return nil
}

func (s InvocationSpec) Normalized() InvocationSpec {
	s.StartedAt = s.StartedAt.UTC()
	return s
}

type InvocationCounters struct {
	Attempted        int64
	Succeeded        int64
	Failed           int64
	Truncated        int64
	Skipped          int64
	Requested        int64
	Started          int64
	Suppressed       int64
	IdentityRejected int64
}

func (c InvocationCounters) Validate(kind Kind) error {
	values := []int64{c.Attempted, c.Succeeded, c.Failed, c.Truncated, c.Skipped,
		c.Requested, c.Started, c.Suppressed, c.IdentityRejected}
	for _, value := range values {
		if value < 0 {
			return errors.New("operation invocation counters must be nonnegative")
		}
	}
	if !isRecordedInvocationKind(kind) && kind != KindPersonEnrichment {
		return fmt.Errorf("operation kind %q has no invocation counters", kind)
	}
	switch kind {
	case KindMessageEmbedding, KindPersonEmbedding:
		if c.Skipped != 0 || c.Requested != 0 || c.Started != 0 || c.Suppressed != 0 || c.IdentityRejected != 0 {
			return fmt.Errorf("operation kind %q has counters outside its allowlist", kind)
		}
	case KindDocumentExtraction, KindDocumentEmbedding:
		if c.Truncated != 0 || c.Skipped != 0 || c.Requested != 0 || c.Started != 0 ||
			c.Suppressed != 0 || c.IdentityRejected != 0 {
			return fmt.Errorf("operation kind %q has counters outside its allowlist", kind)
		}
	case KindVisualEmbedding:
		if c.Truncated != 0 || c.Requested != 0 || c.Started != 0 || c.Suppressed != 0 || c.IdentityRejected != 0 {
			return fmt.Errorf("operation kind %q has counters outside its allowlist", kind)
		}
	case KindPersonEnrichment:
		if c.Attempted != 0 || c.Truncated != 0 || c.Skipped != 0 {
			return fmt.Errorf("operation kind %q has counters outside its allowlist", kind)
		}
	case KindSourceSync, KindPersonSweep, KindCardDAVSync:
		return fmt.Errorf("operation kind %q has no invocation counters", kind)
	}
	return ValidateCounters(kind, c.PublicCounters(kind))
}

func (c InvocationCounters) ValidateCheckpoint(kind Kind, previous InvocationCounters) error {
	if err := previous.Validate(kind); err != nil {
		return fmt.Errorf("previous operation invocation counters: %w", err)
	}
	if err := c.Validate(kind); err != nil {
		return err
	}
	current := []int64{c.Attempted, c.Succeeded, c.Failed, c.Truncated, c.Skipped,
		c.Requested, c.Started, c.Suppressed, c.IdentityRejected}
	prior := []int64{previous.Attempted, previous.Succeeded, previous.Failed, previous.Truncated, previous.Skipped,
		previous.Requested, previous.Started, previous.Suppressed, previous.IdentityRejected}
	for index := range current {
		if current[index] < prior[index] {
			return errors.New("operation invocation checkpoint cannot decrease counters")
		}
	}
	return nil
}

func (c InvocationCounters) ValidateFinal(kind Kind) error {
	if err := c.Validate(kind); err != nil {
		return err
	}
	switch kind {
	case KindMessageEmbedding, KindPersonEmbedding, KindDocumentExtraction, KindDocumentEmbedding:
		if c.Attempted != c.Succeeded+c.Failed {
			return errors.New("final attempted counter must equal succeeded plus failed")
		}
	case KindVisualEmbedding:
		if c.Attempted != c.Succeeded+c.Failed+c.Skipped {
			return errors.New("final attempted counter must equal succeeded plus failed plus skipped")
		}
	case KindPersonEnrichment, KindSourceSync, KindPersonSweep, KindCardDAVSync:
	}
	return nil
}

func (c InvocationCounters) PublicCounters(kind Kind) []PublicCounter {
	unit := invocationCounterUnit(kind)
	values := []struct {
		name  CounterName
		unit  CounterUnit
		value int64
	}{
		{CounterAttempted, unit, c.Attempted},
		{CounterSucceeded, unit, c.Succeeded},
		{CounterFailed, unit, c.Failed},
		{CounterTruncated, unit, c.Truncated},
		{CounterSkipped, unit, c.Skipped},
		{CounterRequested, CounterUnitPeople, c.Requested},
		{CounterStarted, CounterUnitPeople, c.Started},
		{CounterSuppressed, CounterUnitPeople, c.Suppressed},
		{CounterIdentityRejected, CounterUnitPeople, c.IdentityRejected},
	}
	counters := make([]PublicCounter, 0, len(values))
	allowed := counterUnitsByKind[kind]
	for _, value := range values {
		if value.value == 0 || allowed[value.name] != value.unit {
			continue
		}
		counters = append(counters, PublicCounter{Name: value.name, Unit: value.unit, Value: value.value})
	}
	return counters
}

func InvocationCountersFromPublic(kind Kind, counters []PublicCounter) (InvocationCounters, error) {
	if err := ValidateCounters(kind, counters); err != nil {
		return InvocationCounters{}, err
	}
	var result InvocationCounters
	for _, counter := range counters {
		switch counter.Name {
		case CounterAttempted:
			result.Attempted = counter.Value
		case CounterSucceeded:
			result.Succeeded = counter.Value
		case CounterFailed:
			result.Failed = counter.Value
		case CounterTruncated:
			result.Truncated = counter.Value
		case CounterSkipped:
			result.Skipped = counter.Value
		case CounterRequested:
			result.Requested = counter.Value
		case CounterStarted:
			result.Started = counter.Value
		case CounterSuppressed:
			result.Suppressed = counter.Value
		case CounterIdentityRejected:
			result.IdentityRejected = counter.Value
		case CounterProcessed, CounterAdded, CounterUpdated, CounterItemErrors,
			CounterProjectedWrites, CounterBooks, CounterCreated, CounterRemoved:
			return InvocationCounters{}, fmt.Errorf("counter %q is not an invocation counter", counter.Name)
		}
	}
	return result, nil
}

type BeginResult struct {
	ID          StableID
	Disposition BeginDisposition
	Terminal    *Run
}

type Recorder interface {
	Begin(ctx context.Context, spec InvocationSpec) (BeginResult, error)
	Checkpoint(ctx context.Context, id StableID, counters InvocationCounters) error
	Finish(ctx context.Context, id StableID, counters InvocationCounters, state State, publicError *PublicError) error
}

func DeriveInvocationState(kind Kind, counters InvocationCounters, publicError *PublicError) (State, error) {
	if err := counters.ValidateFinal(kind); err != nil {
		return "", err
	}
	if publicError != nil {
		if err := ValidateInvocationPublicError(kind, publicError); err != nil {
			return "", err
		}
		if publicError.Code == PublicErrorInvocationCancelled {
			return StateCancelled, nil
		}
	}
	if counters.Failed > 0 || publicError != nil {
		if counters.Succeeded > 0 {
			return StatePartial, nil
		}
		return StateFailed, nil
	}
	return StateSucceeded, nil
}

func isRecordedInvocationKind(kind Kind) bool {
	switch kind {
	case KindMessageEmbedding, KindPersonEmbedding, KindDocumentExtraction,
		KindDocumentEmbedding, KindVisualEmbedding:
		return true
	default:
		return false
	}
}

func invocationCounterUnit(kind Kind) CounterUnit {
	switch kind {
	case KindMessageEmbedding:
		return CounterUnitMessages
	case KindPersonEmbedding, KindPersonEnrichment:
		return CounterUnitPeople
	case KindDocumentExtraction:
		return CounterUnitDocuments
	case KindDocumentEmbedding:
		return CounterUnitChunks
	case KindVisualEmbedding:
		return CounterUnitAttachments
	default:
		return ""
	}
}
