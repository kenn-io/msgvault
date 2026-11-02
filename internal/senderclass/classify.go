// Package senderclass derives sender identity classes from independent
// authentication and relationship evidence. A class never authorizes an action
// or establishes the truth or authorship of message content.
package senderclass

import "fmt"

type Class string

const (
	Unverified Class = "unverified"
	Verified   Class = "verified"
	Known      Class = "known"
	Contact    Class = "contact"
	Self       Class = "self"
	Any        Class = "any" // A wake floor, never a classification result.
)

type Relationship string

const (
	RelationshipNone    Relationship = "none"
	RelationshipKnown   Relationship = "known"
	RelationshipContact Relationship = "contact"
)

type AuthState string

const (
	AuthNone AuthState = "none"
	AuthFail AuthState = "fail"
	AuthPass AuthState = "pass"
	// AuthPlatform requires an explicit, provenance-bound caller assertion.
	// A source name or transport label alone cannot establish it.
	AuthPlatform AuthState = "platform"
)

type Basis string

const (
	SourceOwned         Basis = "source_owned"
	AuthFailed          Basis = "auth_failed"
	NoAuthEvidence      Basis = "no_auth_evidence"
	CuratedPerson       Basis = "curated_person"
	OwnerSentTo         Basis = "owner_sent_to"
	DMARCPass           Basis = "dmarc_pass"
	Platform            Basis = "platform"
	InvalidRelationship Basis = "invalid_relationship"
)

type Evidence struct {
	SourceIsFromMe bool
	Auth           AuthState
	Relationship   Relationship
}

type Result struct {
	Class Class `json:"sender_class"`
	Basis Basis `json:"sender_class_basis"`
}

// Classify fails closed on missing or unknown evidence. Source-native
// ownership is the only self signal; address equality is deliberately absent.
func Classify(e Evidence) Result {
	if e.SourceIsFromMe {
		return Result{Self, SourceOwned}
	}
	switch e.Auth {
	case AuthFail:
		return Result{Unverified, AuthFailed}
	case AuthPass, AuthPlatform:
	default:
		return Result{Unverified, NoAuthEvidence}
	}
	switch e.Relationship {
	case RelationshipContact:
		return Result{Contact, CuratedPerson}
	case RelationshipKnown:
		return Result{Known, OwnerSentTo}
	case RelationshipNone:
		if e.Auth == AuthPlatform {
			return Result{Verified, Platform}
		}
		return Result{Verified, DMARCPass}
	default:
		return Result{Unverified, InvalidRelationship}
	}
}

// AtLeast compares the nested ladder. Self never satisfies a wake floor,
// including any. Unknown classes and floors fail closed.
func (c Class) AtLeast(floor Class) bool {
	rank := func(c Class) int {
		switch c {
		case Unverified:
			return 0
		case Verified:
			return 1
		case Known:
			return 2
		case Contact:
			return 3
		default:
			return -1
		}
	}
	actual := rank(c)
	if actual < 0 {
		return false
	}
	if floor == Any {
		return true
	}
	minimum := rank(floor)
	return minimum >= 0 && actual >= minimum
}

func ParseFloor(s string) (Class, error) {
	switch c := Class(s); c {
	case Contact, Known, Verified, Any:
		return c, nil
	default:
		return "", fmt.Errorf("invalid sender class floor %q: expected contact, known, verified, or any", s)
	}
}
