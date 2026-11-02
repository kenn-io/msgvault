package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/senderclass"
)

func FuzzSenderClassReadoutAuthenticationGate(f *testing.F) {
	f.Add("email", "pass", "pass", true)
	f.Add("sms", "pass", "pass", true)
	f.Add("platform", "pass", "pass", true)
	f.Add("email", "fail", "pass", false)
	f.Fuzz(func(t *testing.T, kind, dmarc, dkim string, verified bool) {
		auth := senderClassAuth(&senderauth.Verdict{Kind: kind, DMARC: senderauth.Status(dmarc), DKIM: senderauth.Status(dkim), Verified: verified})
		result := senderclass.Classify(senderclass.Evidence{Auth: auth, Relationship: senderclass.RelationshipContact})
		if result.Class.AtLeast(senderclass.Known) {
			assert.Equal(t, "email", kind)
			assert.True(t, verified)
			assert.Equal(t, "pass", dmarc)
		}
		if kind != "email" {
			assert.Equal(t, senderclass.Unverified, result.Class, "a source label cannot authenticate a sender")
		}
	})
}
