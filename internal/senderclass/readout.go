package senderclass

// EvidenceUnavailable marks a bounded read that could not establish complete
// identity or relationship evidence. It must not be reported as unrelated.
const EvidenceUnavailable Basis = "evidence_unavailable"

// Readout separates a message's authentication-gated class from its exact
// address relationship. An omitted relationship means evidence is unavailable;
// none means the lookup completed and found no qualifying relationship.
// These fields establish neither authorship nor permission to act on content.
type Readout struct {
	SenderClass        Class        `json:"sender_class,omitempty" enum:"unverified,verified,known,contact,self"`
	SenderClassBasis   Basis        `json:"sender_class_basis,omitempty" enum:"source_owned,auth_failed,no_auth_evidence,curated_person,owner_sent_to,dmarc_pass,platform,invalid_relationship,evidence_unavailable"`
	SenderRelationship Relationship `json:"sender_relationship,omitempty" enum:"contact,known,none"`
}

func UnavailableReadout() Readout {
	return Readout{SenderClass: Unverified, SenderClassBasis: EvidenceUnavailable}
}
