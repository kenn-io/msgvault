package personmatchworker

import (
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/personmatchpolicy"
	"go.kenn.io/msgvault/internal/store"
)

var ErrPairNotScorable = errors.New("identity match pair is not eligible for scoring")

// BuildPairPacket removes freeform evidence and unsupported endpoints before
// anything can be sent to the provider. Source IDs stay local to the policy.
func BuildPairPacket(candidate *store.IdentityMatchCandidate) (personmatch.PairPacket, []personmatchpolicy.Signal, error) {
	if candidate == nil || candidate.LeftKind != store.IdentityMatchParticipant ||
		candidate.RightKind != store.IdentityMatchParticipant ||
		candidate.State != store.IdentityMatchStateCandidate || candidate.LeftID == candidate.RightID {
		return personmatch.PairPacket{}, nil, ErrPairNotScorable
	}
	packet := personmatch.PairPacket{SchemaVersion: personmatch.PacketSchemaVersion,
		Left:     personmatch.PairEndpoint{Kind: "participant"},
		Right:    personmatch.PairEndpoint{Kind: "participant"},
		Evidence: []personmatch.Evidence{}}
	var signals []personmatchpolicy.Signal
	for _, evidence := range candidate.Evidence {
		class := identitySignalClass(evidence.EvidenceKind)
		if class == "" {
			continue
		}
		entry := personmatch.Evidence{Class: string(class)}
		if string(class) == string(candidate.Basis) && candidate.NormalizedValue != nil {
			value := strings.TrimSpace(*candidate.NormalizedValue)
			if len(value) <= 256 {
				entry.Value = value
			}
		}
		seen := map[int64]bool{}
		for _, support := range evidence.SourceSupport {
			if support.SourceID <= 0 || support.IsConservative || seen[support.SourceID] {
				continue
			}
			seen[support.SourceID] = true
			signals = append(signals, personmatchpolicy.Signal{Class: class, SourceID: support.SourceID})
		}
		entry.SourceCount = len(seen)
		packet.Evidence = append(packet.Evidence, entry)
	}
	if len(packet.Evidence) == 0 {
		class := identitySignalClass(string(candidate.Basis))
		if class != "" {
			entry := personmatch.Evidence{Class: string(class)}
			if candidate.NormalizedValue != nil {
				value := strings.TrimSpace(*candidate.NormalizedValue)
				if len(value) <= 256 {
					entry.Value = value
				}
			}
			seen := map[int64]bool{}
			for _, support := range candidate.SourceSupport {
				if support.SourceID <= 0 || support.IsConservative || seen[support.SourceID] {
					continue
				}
				seen[support.SourceID] = true
				signals = append(signals, personmatchpolicy.Signal{Class: class, SourceID: support.SourceID})
			}
			entry.SourceCount = len(seen)
			packet.Evidence = append(packet.Evidence, entry)
		}
	}
	return packet, signals, nil
}

func identitySignalClass(kind string) personmatchpolicy.SignalClass {
	switch kind {
	case "stable_provider_id":
		return personmatchpolicy.SignalStableID
	case "email":
		return personmatchpolicy.SignalEmail
	case "phone":
		return personmatchpolicy.SignalPhone
	case "self_declaration", "self_declared_alias":
		return personmatchpolicy.SignalSelfDeclaration
	case "display_name":
		return personmatchpolicy.SignalDisplayName
	default:
		return ""
	}
}
