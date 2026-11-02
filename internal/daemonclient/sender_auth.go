package daemonclient

import (
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func senderAuthFromGenerated(v *generated.Verdict) *senderauth.Verdict {
	if v == nil {
		return nil
	}
	result := &senderauth.Verdict{Kind: v.Kind, SPF: senderauth.Status(stringValue(v.Spf)), DKIM: senderauth.Status(stringValue(v.Dkim)), DMARC: senderauth.Status(stringValue(v.Dmarc)), Verified: v.Verified, VerifiedDomain: stringValue(v.VerifiedDomain), AuthservID: stringValue(v.AuthservID), Basis: senderauth.Basis(v.Basis)}
	if result.Kind == "email" {
		if result.SPF == "" {
			result.SPF = senderauth.Unknown
		}
		if result.DKIM == "" {
			result.DKIM = senderauth.Unknown
		}
		if result.DMARC == "" {
			result.DMARC = senderauth.Unknown
		}
	}
	return result
}
