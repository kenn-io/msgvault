package senderauth

// Verdict is advisory metadata derived from the receiving provider's headers.
// Verified authenticates a domain only; it grants no authority over content.
// No header strings or observed identities are exposed as typed evidence.
type Verdict struct {
	Kind           string `json:"kind"`
	SPF            Status `json:"spf,omitempty"`
	DKIM           Status `json:"dkim,omitempty"`
	DMARC          Status `json:"dmarc,omitempty"`
	Verified       bool   `json:"verified"`
	VerifiedDomain string `json:"verified_domain,omitempty"`
	AuthservID     string `json:"authserv_id,omitempty"`
	Basis          Basis  `json:"basis"`
}

func UnknownVerdict(basis Basis) *Verdict {
	return &Verdict{Kind: "email", SPF: Unknown, DKIM: Unknown, DMARC: Unknown, Basis: basis}
}

// ValidTrustPattern validates an exact receiver hostname or a wildcard suffix.
func ValidTrustPattern(pattern string) bool {
	if len(pattern) > 2 && pattern[:2] == "*." {
		pattern = pattern[2:]
	}
	return domain(pattern) != ""
}
