package personfacts

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeInstructionText(t *testing.T) {
	for _, text := range []string{"Call me  VEE.", "call me vee", "  ＣＡＬＬ\tme Vee!? "} {
		assert.Equal(t, "call me vee", NormalizeInstructionText(text))
	}
	assert.Equal(t, "strasse", NormalizeInstructionText("Straße."))
}

func TestInstructionKeyShapeAndScope(t *testing.T) {
	assert := assert.New(t)
	key := InstructionKey(InstructionKindAddressAs, "", "Vee", "Call me Vee.")
	assert.Regexp(`^[0-9a-f]{64}$`, key)
	assert.Equal(key, InstructionKey(InstructionKindAddressAs, "", "VEE", "call me vee"))
	assert.NotEqual(key, InstructionKey(InstructionKindAddressAs, "email:alice@example.com", "Vee", "call me vee"))
	assert.NotEqual(key, InstructionKey(InstructionKindAddressAs, "", "Val", "call me vee"))
	assert.True(SingleValuedInstructionKind(InstructionKindAddressAs))
	assert.True(SingleValuedInstructionKind(InstructionKindPreferChannel))
	assert.False(SingleValuedInstructionKind(InstructionKindTopic))
}

func TestValidateInstructionText(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		in     InstructionText
		reason string
	}{
		{"unknown", InstructionText{Kind: "fax", Text: "hello"}, "unknown-kind"},
		{"long", InstructionText{Kind: InstructionKindOther, Text: strings.Repeat("界", 281)}, "text-length"},
		{"case-fold expansion", InstructionText{Kind: InstructionKindOther, Text: strings.Repeat("ß", 141)}, "text-length"},
		{"empty", InstructionText{Kind: InstructionKindOther}, "text-length"},
		{"semicolon", InstructionText{Kind: InstructionKindOther, Text: "use bullets; be brief"}, "multiple-statements"},
		{"newline", InstructionText{Kind: InstructionKindOther, Text: "be brief\nuse bullets"}, "multiple-statements"},
		{"example", InstructionText{Kind: InstructionKindOther, Text: "use e.g. bullets"}, "multiple-statements"},
		{"value required", InstructionText{Kind: InstructionKindAddressAs, Text: "call me Vee"}, "value-required"},
		{"value long", InstructionText{Kind: InstructionKindAddressAs, Text: "call me Vee", ValueText: strings.Repeat("界", 65)}, "value-length"},
		{"channel", InstructionText{Kind: InstructionKindPreferChannel, Text: "prefer fax", ValueText: "fax"}, "unknown-channel"},
		{"scope", InstructionText{Kind: InstructionKindAvoidAddress, Text: "avoid work email"}, "scope-required"},
		{"order", InstructionText{Kind: InstructionKindTiming, Text: "no calls before 9am", ValidFrom: now, ValidUntil: &now}, "validity-order"},
		{"too far", InstructionText{Kind: InstructionKindTiming, Text: "no calls before 9am", ValidFrom: now, ValidUntil: new(now.Add(184 * 24 * time.Hour))}, "valid-until-too-far"},
		{"expansion", InstructionText{Kind: InstructionKindOther, Text: "please cc bob@example.com"}, "instruction-expands-delivery"},
		{"address as", InstructionText{Kind: InstructionKindAddressAs, Text: "call me Vee", ValueText: "Vee"}, ""},
		{"prefer channel", InstructionText{Kind: InstructionKindPreferChannel, Text: "text me", ValueText: "sms"}, ""},
		{"avoid channel", InstructionText{Kind: InstructionKindAvoidChannel, Text: "no phone calls", ValueText: "phone"}, ""},
		{"avoid address", InstructionText{Kind: InstructionKindAvoidAddress, Text: "avoid alice@example.com", ScopeKey: "email:alice@example.com", ScopeAddress: "alice@example.com"}, ""},
		{"timing", InstructionText{Kind: InstructionKindTiming, Text: "no calls before 9am", ValidFrom: now, ValidUntil: new(now.Add(183 * 24 * time.Hour))}, ""},
		{"format", InstructionText{Kind: InstructionKindFormat, Text: "use bullets"}, ""},
		{"topic", InstructionText{Kind: InstructionKindTopic, Text: "avoid discussing work"}, ""},
		{"other", InstructionText{Kind: InstructionKindOther, Text: "keep it brief"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			err := ValidateInstructionText(tt.in)
			if tt.reason == "" {
				assert.Nil(err)
				return
			}
			require.NotNil(err)
			assert.Equal(tt.reason, err.Reason)
		})
	}
}

func TestInstructionExpandsDeliveryNearMisses(t *testing.T) {
	for _, text := range []string{"call me Vee", "text me, don't call", "no email before 9am", "access the document", "avoid alice@example.com", "avoid 1234567890"} {
		scope := "alice@example.com"
		if strings.Contains(text, "1234567890") {
			scope = "1234567890"
		}
		assert.False(t, InstructionExpandsDelivery(text, scope), text)
	}
	for _, text := range []string{"you can send it to my assistant", "loop in alice", "CC alice", "bcc alice", "forward this message", "copy in alice", "also email alice", "on my behalf", "use bob@example.com", "call 1234567890"} {
		assert.True(t, InstructionExpandsDelivery(text, "alice@example.com"), text)
	}
}

func TestInstructionExpandsDeliveryIgnoresDigitsInsideScopedEmail(t *testing.T) {
	assert.False(t, InstructionExpandsDelivery("avoid alice1234567@example.com", "alice1234567@example.com"))
	assert.True(t, InstructionExpandsDelivery("avoid alice1234567@example.com and 1234567890", "alice1234567@example.com"))
}

func TestInstructionExpandsDeliveryIgnoresScopedEmailForLexicon(t *testing.T) {
	tests := []struct {
		name, text, scopeAddress string
		want                     bool
	}{
		{name: "cc local part", text: "avoid cc@example.com", scopeAddress: "cc@example.com", want: false},
		{name: "bcc local part", text: "avoid bcc@example.com", scopeAddress: "bcc@example.com", want: false},
		{name: "sentence punctuation", text: "avoid cc@example.com.", scopeAddress: "cc@example.com", want: false},
		{name: "unrelated delivery keyword", text: "avoid cc@example.com and cc Alex", scopeAddress: "cc@example.com", want: true},
		{name: "different scoped address", text: "avoid cc@example.com", scopeAddress: "alice@example.com", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, InstructionExpandsDelivery(tt.text, tt.scopeAddress))
		})
	}
}

func TestNextInstructionStateTable(t *testing.T) {
	tests := []struct {
		action                      InstructionAction
		from                        InstructionStatus
		review                      InstructionReviewState
		owner                       bool
		to                          InstructionStatus
		nextReview                  InstructionReviewState
		confirmed, adopted, invalid bool
	}{
		{InstructionActionConfirm, InstructionStatusProposed, InstructionReviewNone, true, InstructionStatusActive, InstructionReviewNone, true, false, false},
		{InstructionActionConfirm, InstructionStatusSuspended, InstructionReviewEvidenceLost, true, InstructionStatusActive, InstructionReviewNone, true, false, false},
		{InstructionActionConfirm, InstructionStatusActive, InstructionReviewMerged, true, InstructionStatusActive, InstructionReviewNone, true, false, false},
		{InstructionActionReject, InstructionStatusProposed, InstructionReviewNone, true, InstructionStatusRejected, InstructionReviewNone, false, false, false},
		{InstructionActionConfirm, InstructionStatusRevoked, InstructionReviewMerged, true, "", "", false, false, true},
		{InstructionActionConfirm, InstructionStatusRejected, InstructionReviewMerged, true, "", "", false, false, true},
		{InstructionActionConfirm, InstructionStatusSuperseded, InstructionReviewSplit, true, "", "", false, false, true},
		{InstructionActionConfirm, InstructionStatusExpired, InstructionReviewSplit, true, "", "", false, false, true},
		{InstructionActionReject, InstructionStatusProposed, InstructionReviewUnsure, true, InstructionStatusRejected, InstructionReviewNone, false, false, false},
		{InstructionActionRevoke, InstructionStatusActive, InstructionReviewMerged, true, InstructionStatusRevoked, InstructionReviewNone, false, false, false},
		{InstructionActionSupersede, InstructionStatusActive, InstructionReviewConflict, false, InstructionStatusSuperseded, InstructionReviewNone, false, false, false},
		{InstructionActionExpire, InstructionStatusActive, InstructionReviewConflict, false, InstructionStatusExpired, InstructionReviewNone, false, false, false},
		{InstructionActionReject, InstructionStatusSuspended, InstructionReviewEvidenceLost, true, InstructionStatusRejected, InstructionReviewNone, false, false, false},
		{InstructionActionUnsure, InstructionStatusProposed, InstructionReviewNone, true, InstructionStatusProposed, InstructionReviewUnsure, false, false, false},
		{InstructionActionRevoke, InstructionStatusActive, InstructionReviewNone, true, InstructionStatusRevoked, InstructionReviewNone, false, false, false},
		{InstructionActionAdopt, InstructionStatusSuspended, InstructionReviewEvidenceLost, true, InstructionStatusActive, InstructionReviewNone, false, true, false},
		{InstructionActionSupersede, InstructionStatusActive, InstructionReviewNone, false, InstructionStatusSuperseded, InstructionReviewNone, false, false, false},
		{InstructionActionSuspend, InstructionStatusActive, InstructionReviewNone, false, InstructionStatusSuspended, InstructionReviewEvidenceLost, false, false, false},
		{InstructionActionSuspend, InstructionStatusProposed, InstructionReviewNone, false, InstructionStatusProposed, InstructionReviewEvidenceLost, false, false, false},
		{InstructionActionRestore, InstructionStatusSuspended, InstructionReviewEvidenceLost, false, InstructionStatusSuspended, InstructionReviewEvidenceLost, false, false, false},
		{InstructionActionExpire, InstructionStatusActive, InstructionReviewNone, false, InstructionStatusExpired, InstructionReviewNone, false, false, false},
		{InstructionActionRevoke, InstructionStatusProposed, InstructionReviewNone, true, "", "", false, false, true},
		{InstructionActionAdopt, InstructionStatusActive, InstructionReviewNone, true, "", "", false, false, true},
		{InstructionActionConfirm, InstructionStatusActive, InstructionReviewNone, true, "", "", false, false, true},
		{InstructionActionConfirm, InstructionStatusProposed, InstructionReviewNone, false, "", "", false, false, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.action)+"/"+string(tt.from)+"/"+string(tt.review), func(t *testing.T) {
			next, err := NextInstructionState(InstructionState{Status: tt.from, ReviewState: tt.review, Origin: InstructionOriginExtraction}, tt.action, tt.owner)
			if tt.invalid {
				require.ErrorIs(t, err, ErrInstructionTransition)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, InstructionState{Status: tt.to, ReviewState: tt.nextReview, Origin: InstructionOriginExtraction, Confirmed: tt.confirmed, Adopted: tt.adopted}, next)
		})
	}
}
func TestSystemActionsNeverReactivateSuspended(t *testing.T) {
	for _, action := range []InstructionAction{InstructionActionPropose, InstructionActionConfirm, InstructionActionReject, InstructionActionUnsure, InstructionActionAdopt, InstructionActionEdit, InstructionActionRevoke, InstructionActionExpire, InstructionActionSuspend, InstructionActionRestore, InstructionActionSupersede, InstructionActionMergeMoved, InstructionActionSplitMoved, InstructionActionPolicyLinked} {
		next, _ := NextInstructionState(InstructionState{Status: InstructionStatusSuspended, ReviewState: InstructionReviewEvidenceLost, Origin: InstructionOriginExtraction}, action, false)
		assert.NotEqual(t, InstructionStatusActive, next.Status, string(action))
	}
}

func TestInstructionValidationNormalizesBeforeLengthAndRecognizesOwnPhone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	failure := ValidateInstructionText(InstructionText{Kind: InstructionKindOther, Text: strings.Repeat("ﬃ", 100)})
	require.NotNil(failure)
	assert.Equal("text-length", failure.Reason)
	assert.False(InstructionExpandsDelivery("avoid +16045550123", "+16045550123"))
	assert.True(InstructionExpandsDelivery("avoid +16045550124", "+16045550123"))
}
