package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SenderClassCeiling is relationship evidence for an exact mailbox identity.
// It never authenticates a person or a message, or authorizes an action.
type SenderClassCeiling string

const (
	SenderCeilingContact SenderClassCeiling = "contact"
	SenderCeilingKnown   SenderClassCeiling = "known"
	SenderCeilingNone    SenderClassCeiling = "none"
)

type SenderClassCeilings struct {
	Participants map[int64]SenderClassCeiling
	Persons      map[int64]SenderClassCeiling
}

// SenderClassCeilings derives ceilings from exact participant primary addresses
// and explicitly bound person members. Link clusters, aliases, structured
// contact points, names, profile text, and activity tiers cannot transfer trust.
// The entire bounded lookup shares one snapshot and source scope. A missing
// mailbox has no qualifying address evidence; errors return no partial result.
func (s *Store) SenderClassCeilings(ctx context.Context, participantIDs, personIDs, allowedSourceIDs []int64) (SenderClassCeilings, error) {
	if err := ctx.Err(); err != nil {
		return SenderClassCeilings{}, err
	}
	if len(participantIDs)+len(personIDs) > 500 {
		return SenderClassCeilings{}, ErrSenderRelationshipLimit
	}
	out := SenderClassCeilings{Participants: map[int64]SenderClassCeiling{}, Persons: map[int64]SenderClassCeiling{}}
	var values []string
	var args []any
	for kind, ids := range map[string][]int64{"participant": participantIDs, "person": personIDs} {
		for _, id := range ids {
			if id <= 0 {
				return SenderClassCeilings{}, errors.New("sender ceiling target id must be positive")
			}
			targets := out.Participants
			if kind == "person" {
				targets = out.Persons
			}
			if _, exists := targets[id]; exists {
				continue
			}
			targets[id] = SenderCeilingNone
			values = append(values, "('"+kind+"',CAST(? AS BIGINT))")
			args = append(args, id)
		}
	}
	if len(values) == 0 {
		return out, nil
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return SenderClassCeilings{}, err
	}
	defer release()
	rows, err := ReadDBContext(ctx, s.DB()).QueryContext(ctx, s.Rebind(`WITH requested(kind,target_id) AS (VALUES `+strings.Join(values, ",")+`)
		SELECT r.kind,r.target_id,LOWER(TRIM(COALESCE(p.email_address,''))) FROM requested r
		JOIN participants p ON r.kind='participant' AND p.id=r.target_id
		UNION ALL
		SELECT r.kind,r.target_id,LOWER(TRIM(COALESCE(p.email_address,''))) FROM requested r
		JOIN person_participants pp ON r.kind='person' AND pp.person_id=r.target_id JOIN participants p ON p.id=pp.participant_id
		LIMIT 5001`), args...)
	if err != nil {
		return SenderClassCeilings{}, fmt.Errorf("read sender ceiling identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type target struct {
		kind    string
		id      int64
		address string
	}
	var targets []target
	var addresses []string
	for rows.Next() {
		var row target
		if err := rows.Scan(&row.kind, &row.id, &row.address); err != nil {
			return SenderClassCeilings{}, err
		}
		targets = append(targets, row)
		if len(targets) > 5000 {
			return SenderClassCeilings{}, ErrSenderRelationshipLimit
		}
		if row.address != "" {
			addresses = append(addresses, row.address)
		}
	}
	if err := rows.Err(); err != nil {
		return SenderClassCeilings{}, err
	}
	_ = rows.Close()
	slices.Sort(addresses)
	addresses = slices.Compact(addresses)
	evidence := map[string]SenderRelationshipEvidence{}
	for start := 0; start < len(addresses); start += MaxSenderRelationshipAddresses {
		batch, err := s.SenderRelationships(ctx, addresses[start:min(start+MaxSenderRelationshipAddresses, len(addresses))], allowedSourceIDs)
		if err != nil {
			return SenderClassCeilings{}, err
		}
		for address, relationship := range batch {
			evidence[address] = relationship
		}
	}
	for _, target := range targets {
		ceiling := SenderClassCeiling(evidence[target.address].Relationship)
		if ceiling != SenderCeilingContact && ceiling != SenderCeilingKnown {
			continue
		}
		result := out.Participants
		if target.kind == "person" {
			result = out.Persons
		}
		if result[target.id] != SenderCeilingContact {
			result[target.id] = ceiling
		}
	}
	return out, nil
}

// SenderCeilingBasis describes relationship evidence without authenticating mail.
type SenderCeilingBasis string

const (
	SenderCeilingCuratedPerson  SenderCeilingBasis = "curated_person"
	SenderCeilingOwnerSentTo    SenderCeilingBasis = "owner_sent_to"
	SenderCeilingNoRelationship SenderCeilingBasis = "no_relationship"
)

// SenderCeilingReadout is omitted when a bounded lookup is unavailable.
type SenderCeilingReadout struct {
	SenderClassCeiling      SenderClassCeiling `json:"sender_class_ceiling,omitempty" enum:"contact,known,none"`
	SenderClassCeilingBasis SenderCeilingBasis `json:"sender_class_ceiling_basis,omitempty" enum:"curated_person,owner_sent_to,no_relationship"`
}

func SenderCeiling(ceiling SenderClassCeiling) SenderCeilingReadout {
	switch ceiling {
	case SenderCeilingContact:
		return SenderCeilingReadout{ceiling, SenderCeilingCuratedPerson}
	case SenderCeilingKnown:
		return SenderCeilingReadout{ceiling, SenderCeilingOwnerSentTo}
	case SenderCeilingNone:
		return SenderCeilingReadout{ceiling, SenderCeilingNoRelationship}
	default:
		return SenderCeilingReadout{}
	}
}
