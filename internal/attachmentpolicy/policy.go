// Package attachmentpolicy evaluates provider-neutral media download policy.
package attachmentpolicy

import "fmt"

// DefaultChatMaxBytes is the per-attachment size cap chat providers (Beeper,
// Slack, Teams) apply when max_media_mb is unset. It is sized for the media
// worth keeping from direct chats and small groups: long voice notes, screen
// recordings, and phone video routinely clear 100 MiB, and the participant
// cap already keeps large-room volume out. Discord keeps its own lower cap.
const DefaultChatMaxBytes int64 = 250 << 20

// Scope limits media downloads by conversation kind.
type Scope string

const (
	ScopeAll    Scope = "all"
	ScopeDirect Scope = "direct"
	ScopeNone   Scope = "none"
)

// SkipReason explains why an attachment occurrence was not stored.
type SkipReason string

const (
	SkipPolicyScope          SkipReason = "policy_scope"
	SkipParticipantThreshold SkipReason = "participant_threshold"
	SkipAccountPolicy        SkipReason = "account_policy"
	SkipSizeCap              SkipReason = "size_cap"
	SkipFetchFailure         SkipReason = "fetch_failure"
)

// DownloadState is the durable outcome of provider attachment processing.
type DownloadState string

const (
	StatePending DownloadState = "pending"
	StateStored  DownloadState = "stored"
	StateSkipped DownloadState = "skipped"
	StateFailed  DownloadState = "failed"
)

// Conversation is the provider-neutral context used to evaluate scope.
type Conversation struct {
	Type             string
	ParticipantCount int
}

// Policy is the resolved provider/account media policy. Its zero value allows
// all conversations and sizes for backward compatibility.
type Policy struct {
	Scope           Scope
	MaxParticipants int
	MaxBytes        int64
	DisabledReason  SkipReason
}

// Validate rejects values that cannot be evaluated consistently.
func (p Policy) Validate() error {
	switch p.Scope {
	case "", ScopeAll, ScopeDirect, ScopeNone:
	default:
		return fmt.Errorf("invalid media_scope %q (want all, direct, or none)", p.Scope)
	}
	if p.MaxParticipants < 0 {
		return fmt.Errorf("invalid media_max_participants %d: must be zero or positive", p.MaxParticipants)
	}
	if p.MaxBytes < 0 {
		return fmt.Errorf("invalid max_media_mb: byte limit %d must be zero or positive", p.MaxBytes)
	}
	return nil
}

// Evaluate returns the first policy reason that excludes an occurrence.
func (p Policy) Evaluate(conversation Conversation, size int64) SkipReason {
	if p.DisabledReason != "" {
		return p.DisabledReason
	}
	switch p.Scope {
	case ScopeAll:
		// Every conversation type is eligible; only the shared limits below apply.
	case ScopeNone:
		return SkipPolicyScope
	case ScopeDirect:
		if conversation.Type != "direct_chat" && conversation.Type != "group_chat" {
			return SkipPolicyScope
		}
	}
	if p.MaxParticipants > 0 && conversation.ParticipantCount > p.MaxParticipants {
		return SkipParticipantThreshold
	}
	if p.MaxBytes > 0 && size > p.MaxBytes {
		return SkipSizeCap
	}
	return ""
}

// Allows reports whether an occurrence is permitted by the resolved policy.
func (p Policy) Allows(conversation Conversation, size int64) bool {
	return p.Evaluate(conversation, size) == ""
}

// RetryEligible reports whether a stored outcome represents unfinished work.
// Empty state is eligible for backward compatibility with legacy markers.
func RetryEligible(state DownloadState) bool {
	return state == "" || state == StatePending || state == StateFailed
}

// OversizeMarkerSize returns a safely representable observation that remains
// above maxBytes. Providers use it when a bounded stream proves the source's
// declared size was missing or understated.
func OversizeMarkerSize(maxBytes, observed int64) int {
	maxInt := int64(^uint(0) >> 1)
	if observed <= maxBytes && maxBytes < maxInt {
		observed = maxBytes + 1
	}
	if observed > maxInt {
		observed = maxInt
	}
	if observed < 0 {
		return 0
	}
	return int(observed)
}
