package daemonclient

import "go.kenn.io/msgvault/internal/senderclass"

func senderClassFromGenerated[C, B, R ~string](class *C, basis *B, relationship *R) senderclass.Readout {
	out := senderclass.Readout{SenderClass: senderclass.Class(enumString(class)), SenderClassBasis: senderclass.Basis(enumString(basis)), SenderRelationship: senderclass.Relationship(enumString(relationship))}
	switch out.SenderClass {
	case senderclass.Unverified, senderclass.Verified, senderclass.Known, senderclass.Contact, senderclass.Self:
	default:
		return senderclass.UnavailableReadout()
	}
	switch out.SenderClassBasis {
	case senderclass.SourceOwned, senderclass.AuthFailed, senderclass.NoAuthEvidence, senderclass.CuratedPerson, senderclass.OwnerSentTo, senderclass.DMARCPass, senderclass.Platform, senderclass.InvalidRelationship:
	default:
		return senderclass.UnavailableReadout()
	}
	switch out.SenderRelationship {
	case senderclass.RelationshipNone, senderclass.RelationshipKnown, senderclass.RelationshipContact:
	default:
		return senderclass.UnavailableReadout()
	}
	return out
}

func enumString[T ~string](value *T) string {
	if value == nil {
		return ""
	}
	return string(*value)
}
