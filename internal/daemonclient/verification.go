package daemonclient

import (
	"slices"

	"go.kenn.io/msgvault/internal/verification"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// VerificationMinAPISchemaVersion adds verification metadata and source-scoped
// verification masking on delegated reads.
const VerificationMinAPISchemaVersion = "3.36.0"

func verificationFromGenerated(m *generated.Metadata) *verification.Metadata {
	if m == nil {
		return nil
	}
	return &verification.Metadata{
		Kind: m.Kind, Confidence: m.Confidence, Signals: slices.Clone(m.Signals),
		Sender: stringValue(m.Sender), ReceivedAt: m.ReceivedAt, ExpiresAt: copyTime(m.ExpiresAt),
		Expired: m.Expired, Hosts: slices.Clone(m.Hosts), Masked: m.Masked,
		MaskedFields: slices.Clone(m.MaskedFields), Reveal: m.Reveal,
	}
}
