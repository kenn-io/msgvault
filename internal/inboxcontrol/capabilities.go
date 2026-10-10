package inboxcontrol

import (
	"context"
	"slices"
	"time"
)

// CapabilityStatus distinguishes a native limitation from missing authority or
// missing evidence. Capabilities describe current support; they do not grant it.
type CapabilityStatus string

const (
	CapabilitySupported          CapabilityStatus = "supported"
	CapabilityUnsupported        CapabilityStatus = "unsupported"
	CapabilityPermissionRequired CapabilityStatus = "permission-required"
	CapabilityUnavailable        CapabilityStatus = "unavailable"
)

type Capability struct {
	Operation Operation        `json:"operation"`
	Status    CapabilityStatus `json:"status"`
	Reason    string           `json:"reason,omitempty"`
}

// Capabilities is metadata only. LocationModel distinguishes Gmail labels from
// real mailboxes and chat archive flags. ConditionalWrite describes native CAS,
// independently of the daemon's signed preflight and execution lease.
type Capabilities struct {
	Source           SourceIdentity `json:"source"`
	LocationModel    string         `json:"location_model"`
	ConditionalWrite bool           `json:"conditional_write"`
	ObservedAt       time.Time      `json:"observed_at"`
	Operations       []Capability   `json:"operations"`
}

// CapabilityProvider probes current provider evidence without a mutation.
type CapabilityProvider interface {
	Capabilities(ctx context.Context, request Request) (*Capabilities, error)
}

func (s *Service) readCapabilities(ctx context.Context, provider Provider, request Request, principal Principal) (*Result, error) {
	reader, ok := provider.(CapabilityProvider)
	if !ok {
		return nil, ErrUnavailable
	}
	observed, err := reader.Capabilities(ctx, request)
	if err != nil {
		return nil, safePreflightError(err)
	}
	if observed == nil || request.Source == nil || observed.Source != *request.Source || observed.ObservedAt.IsZero() {
		return nil, ErrUnavailable
	}
	if len(observed.Operations) == 0 || len(observed.Operations) > 32 {
		return nil, ErrUnavailable
	}
	seen := map[Operation]bool{}
	for _, capability := range observed.Operations {
		if _, err := capability.Operation.RequiredPermission(); err != nil || seen[capability.Operation] {
			return nil, ErrUnavailable
		}
		seen[capability.Operation] = true
		switch capability.Status {
		case CapabilitySupported, CapabilityUnsupported, CapabilityPermissionRequired, CapabilityUnavailable:
		default:
			return nil, ErrUnavailable
		}
	}
	result := *observed
	result.Operations = slices.Clone(observed.Operations)
	for i := range result.Operations {
		capability := &result.Operations[i]
		if capability.Status != CapabilitySupported {
			continue
		}
		intent := request
		intent.Operation = capability.Operation
		if s.authorize(ctx, principal, intent) != nil {
			capability.Status, capability.Reason = CapabilityPermissionRequired, "Source/action grant required"
		}
	}
	return &Result{Capabilities: &result}, nil
}
