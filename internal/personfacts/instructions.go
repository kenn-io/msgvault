package personfacts

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type InstructionKind string

const (
	InstructionKindAddressAs     InstructionKind = "address_as"
	InstructionKindPreferChannel InstructionKind = "prefer_channel"
	InstructionKindAvoidChannel  InstructionKind = "avoid_channel"
	InstructionKindAvoidAddress  InstructionKind = "avoid_address"
	InstructionKindTiming        InstructionKind = "timing"
	InstructionKindFormat        InstructionKind = "format"
	InstructionKindTopic         InstructionKind = "topic"
	InstructionKindOther         InstructionKind = "other"
)

type InstructionStatus string

const (
	InstructionStatusProposed   InstructionStatus = "proposed"
	InstructionStatusActive     InstructionStatus = "active"
	InstructionStatusSuspended  InstructionStatus = "suspended"
	InstructionStatusSuperseded InstructionStatus = "superseded"
	InstructionStatusExpired    InstructionStatus = "expired"
	InstructionStatusRevoked    InstructionStatus = "revoked"
	InstructionStatusRejected   InstructionStatus = "rejected"
)

type InstructionReviewState string

const (
	InstructionReviewNone         InstructionReviewState = "none"
	InstructionReviewUnsure       InstructionReviewState = "unsure"
	InstructionReviewConflict     InstructionReviewState = "conflict"
	InstructionReviewEvidenceLost InstructionReviewState = "evidence_lost"
	InstructionReviewMerged       InstructionReviewState = "merged"
	InstructionReviewSplit        InstructionReviewState = "split"
)

type InstructionOrigin string

const (
	InstructionOriginExtraction InstructionOrigin = "extraction"
	InstructionOriginOwner      InstructionOrigin = "owner"
)

type InstructionAction string

const (
	InstructionActionPropose      InstructionAction = "propose"
	InstructionActionConfirm      InstructionAction = "confirm"
	InstructionActionReject       InstructionAction = "reject"
	InstructionActionUnsure       InstructionAction = "unsure"
	InstructionActionAdopt        InstructionAction = "adopt"
	InstructionActionEdit         InstructionAction = "edit"
	InstructionActionRevoke       InstructionAction = "revoke"
	InstructionActionExpire       InstructionAction = "expire"
	InstructionActionSuspend      InstructionAction = "suspend"
	InstructionActionRestore      InstructionAction = "restore"
	InstructionActionSupersede    InstructionAction = "supersede"
	InstructionActionMergeMoved   InstructionAction = "merge_moved"
	InstructionActionSplitMoved   InstructionAction = "split_moved"
	InstructionActionPolicyLinked InstructionAction = "policy_linked"
)

type InstructionChannel string

const (
	InstructionChannelEmail    InstructionChannel = "email"
	InstructionChannelPhone    InstructionChannel = "phone"
	InstructionChannelSMS      InstructionChannel = "sms"
	InstructionChannelChat     InstructionChannel = "chat"
	InstructionChannelInPerson InstructionChannel = "in_person"
)

// NormalizeInstructionText is the version-one instruction identity normalizer.
func NormalizeInstructionText(s string) string {
	s = cases.Fold().String(norm.NFKC.String(s))
	return strings.TrimSpace(strings.TrimRight(strings.Join(strings.Fields(s), " "), ".,;:!?"))
}

func InstructionKey(kind InstructionKind, scopeKey, valueText, text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("instr-norm-v1\x00"+string(kind)+"\x00"+scopeKey+"\x00"+NormalizeInstructionText(valueText)+"\x00"+NormalizeInstructionText(text))))
}

func (k InstructionKind) Valid() bool {
	switch k {
	case InstructionKindAddressAs, InstructionKindPreferChannel, InstructionKindAvoidChannel, InstructionKindAvoidAddress, InstructionKindTiming, InstructionKindFormat, InstructionKindTopic, InstructionKindOther:
		return true
	default:
		return false
	}
}
func SingleValuedInstructionKind(k InstructionKind) bool {
	return k == InstructionKindAddressAs || k == InstructionKindPreferChannel
}
func (c InstructionChannel) Valid() bool {
	switch c {
	case InstructionChannelEmail, InstructionChannelPhone, InstructionChannelSMS, InstructionChannelChat, InstructionChannelInPerson:
		return true
	default:
		return false
	}
}

type InstructionText struct {
	Kind                                    InstructionKind
	ScopeKey, ScopeAddress, ValueText, Text string
	ValidFrom                               time.Time
	ValidUntil                              *time.Time
}

type InstructionValidationError struct{ Field, Reason string }

func (e *InstructionValidationError) Error() string { return e.Field + ": " + e.Reason }

func ValidateInstructionText(in InstructionText) *InstructionValidationError {
	fail := func(field, reason string) *InstructionValidationError {
		return &InstructionValidationError{Field: field, Reason: reason}
	}
	if !in.Kind.Valid() {
		return fail("kind", "unknown-kind")
	}
	text := strings.TrimSpace(norm.NFKC.String(in.Text))
	normalizedLength := utf8.RuneCountInString(NormalizeInstructionText(text))
	if n := utf8.RuneCountInString(text); n < 1 || n > 280 || normalizedLength < 1 || normalizedLength > 280 {
		return fail("text", "text-length")
	}
	if strings.ContainsAny(in.Text, ";\n\r\u2028\u2029") || strings.ContainsAny(text, ";\n\r\u2028\u2029") || strings.Contains(cases.Fold().String(text), "e.g.") {
		return fail("text", "multiple-statements")
	}
	value := strings.TrimSpace(in.ValueText)
	if (in.Kind == InstructionKindAddressAs || in.Kind == InstructionKindPreferChannel || in.Kind == InstructionKindAvoidChannel) && value == "" {
		return fail("value", "value-required")
	}
	if utf8.RuneCountInString(value) > 64 {
		return fail("value", "value-length")
	}
	if (in.Kind == InstructionKindPreferChannel || in.Kind == InstructionKindAvoidChannel) && !InstructionChannel(value).Valid() {
		return fail("value", "unknown-channel")
	}
	if in.Kind == InstructionKindAvoidAddress && (in.ScopeKey == "" || in.ScopeAddress == "") {
		return fail("contact_point_id", "scope-required")
	}
	if in.ValidUntil != nil {
		if !in.ValidUntil.After(in.ValidFrom) {
			return fail("valid_until", "validity-order")
		}
		if in.ValidUntil.Sub(in.ValidFrom) > 183*24*time.Hour {
			return fail("valid_until", "valid-until-too-far")
		}
	}
	if InstructionExpandsDelivery(text, in.ScopeAddress) || InstructionExpandsDelivery(value, in.ScopeAddress) {
		return fail("text", "instruction-expands-delivery")
	}
	return nil
}

var instructionDeliveryLexicon = regexp.MustCompile(`\b(?:cc|bcc|forward (?:to|it|this)|copy in|loop in|also (?:send|email)|you (?:may|can) send|allowed to send|permission to send|on my behalf)\b`)
var instructionEmailToken = regexp.MustCompile(`[^\s<>()\[\]{},;:!?"']+@[^\s<>()\[\]{},;:!?"']+`)
var instructionDigits = regexp.MustCompile(`[0-9]{7,}`)

// InstructionExpandsDelivery recognizes only the closed delivery-expansion lexicon.
func InstructionExpandsDelivery(text, scopeAddress string) bool {
	text = cases.Fold().String(norm.NFKC.String(text))
	text = strings.Join(strings.Fields(text), " ")
	scopeAddress = NormalizeInstructionText(scopeAddress)
	for _, token := range instructionEmailToken.FindAllString(text, -1) {
		if NormalizeInstructionText(token) != scopeAddress {
			return true
		}
	}
	textWithoutEmailTokens := instructionEmailToken.ReplaceAllString(text, " ")
	if instructionDeliveryLexicon.MatchString(textWithoutEmailTokens) {
		return true
	}
	for _, token := range instructionDigits.FindAllString(textWithoutEmailTokens, -1) {
		if token != strings.TrimPrefix(scopeAddress, "+") {
			return true
		}
	}
	return false
}

type InstructionState struct {
	Status             InstructionStatus
	ReviewState        InstructionReviewState
	Origin             InstructionOrigin
	Confirmed, Adopted bool
}

var ErrInstructionTransition = errors.New("instruction transition is not allowed")

func NextInstructionState(cur InstructionState, action InstructionAction, owner bool) (InstructionState, error) {
	next := cur
	switch action {
	case InstructionActionConfirm:
		if !owner {
			return cur, ErrInstructionTransition
		}
		if cur.Status != InstructionStatusProposed && cur.Status != InstructionStatusSuspended && (cur.Status != InstructionStatusActive || cur.ReviewState == InstructionReviewNone) {
			return cur, ErrInstructionTransition
		}
		next.Status = InstructionStatusActive
		next.ReviewState = InstructionReviewNone
		next.Confirmed = true
	case InstructionActionReject:
		if !owner || (cur.Status != InstructionStatusProposed && cur.Status != InstructionStatusSuspended) {
			return cur, ErrInstructionTransition
		}
		next.Status = InstructionStatusRejected
		next.ReviewState = InstructionReviewNone
	case InstructionActionUnsure:
		if !owner || cur.Status != InstructionStatusProposed {
			return cur, ErrInstructionTransition
		}
		next.ReviewState = InstructionReviewUnsure
	case InstructionActionRevoke:
		if !owner || cur.Status != InstructionStatusActive {
			return cur, ErrInstructionTransition
		}
		next.Status = InstructionStatusRevoked
		next.ReviewState = InstructionReviewNone
	case InstructionActionAdopt:
		if !owner || cur.Status != InstructionStatusSuspended {
			return cur, ErrInstructionTransition
		}
		next.Status = InstructionStatusActive
		next.Adopted = true
		next.ReviewState = InstructionReviewNone
	case InstructionActionSupersede:
		if cur.Status != InstructionStatusActive {
			return cur, ErrInstructionTransition
		}
		next.Status = InstructionStatusSuperseded
		next.ReviewState = InstructionReviewNone
	case InstructionActionSuspend:
		if owner || (cur.Status != InstructionStatusActive && cur.Status != InstructionStatusProposed) {
			return cur, ErrInstructionTransition
		}
		if cur.Status == InstructionStatusActive {
			next.Status = InstructionStatusSuspended
		}
		next.ReviewState = InstructionReviewEvidenceLost
	case InstructionActionRestore:
		if owner {
			return cur, ErrInstructionTransition
		}
	case InstructionActionExpire:
		if owner || cur.Status != InstructionStatusActive {
			return cur, ErrInstructionTransition
		}
		next.Status = InstructionStatusExpired
		next.ReviewState = InstructionReviewNone
	default:
		return cur, ErrInstructionTransition
	}
	return next, nil
}
