// Package resolver translates public person references into one attachment
// search population shared by every retrieval lane.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
)

type ReferenceKind string

const (
	ReferencePerson      ReferenceKind = "person"
	ReferenceParticipant ReferenceKind = "participant"
)

type Reference struct {
	Kind ReferenceKind
	ID   int64
}

type Resolution struct {
	Reference Reference
	PersonID  int64
	Scope     personscope.Scope
}

var (
	ErrUnavailable     = errors.New("person scope resolution is unavailable")
	ErrEmptyPopulation = errors.New("person has no resolved identities")
)

type DurablePersonStore interface {
	GetPersonContext(ctx context.Context, id int64) (*store.Person, error)
}

type ClusterStore interface {
	ClusterMembers(id int64) ([]int64, error)
}

type BindingStore interface {
	PersonForParticipantsContext(ctx context.Context, participantIDs []int64) (*store.Person, error)
}

func Resolve(
	ctx context.Context,
	profiles any,
	reference Reference,
	directions []personscope.Direction,
) (Resolution, error) {
	if profiles == nil {
		return Resolution{}, ErrUnavailable
	}
	if reference.ID <= 0 {
		return Resolution{}, errors.New("person reference ID must be positive")
	}
	normalizedDirections, defaultDirections, err := NormalizeDirections(directions)
	if err != nil {
		return Resolution{}, err
	}
	resolved := Resolution{Reference: reference}
	switch reference.Kind {
	case ReferencePerson:
		durable, ok := profiles.(DurablePersonStore)
		if !ok {
			return Resolution{}, ErrUnavailable
		}
		person, getErr := durable.GetPersonContext(ctx, reference.ID)
		if getErr != nil {
			return Resolution{}, fmt.Errorf("resolve durable person %d: %w", reference.ID, getErr)
		}
		resolved.PersonID = person.ID
		resolved.Scope.ParticipantIDs = slices.Clone(person.ParticipantIDs)
	case ReferenceParticipant:
		members := []int64{reference.ID}
		if clusters, ok := profiles.(ClusterStore); ok {
			var clusterErr error
			members, clusterErr = clusters.ClusterMembers(reference.ID)
			if clusterErr != nil {
				return Resolution{}, fmt.Errorf("resolve participant %d cluster: %w", reference.ID, clusterErr)
			}
		}
		if len(members) == 0 {
			members = []int64{reference.ID}
		}
		if bindings, ok := profiles.(BindingStore); ok {
			person, personErr := bindings.PersonForParticipantsContext(ctx, members)
			if personErr != nil {
				return Resolution{}, fmt.Errorf("resolve participant %d durable person: %w", reference.ID, personErr)
			}
			if person != nil {
				resolved.PersonID = person.ID
				if len(person.ParticipantIDs) > 0 {
					members = person.ParticipantIDs
				}
			}
		}
		resolved.Scope.ParticipantIDs = slices.Clone(members)
	default:
		return Resolution{}, fmt.Errorf("unknown person reference kind %q", reference.Kind)
	}
	resolved.Scope.ParticipantIDs, err = normalizeParticipantIDs(resolved.Scope.ParticipantIDs)
	if err != nil {
		return Resolution{}, err
	}
	resolved.Scope.Directions = normalizedDirections
	resolved.Scope.IncludeUnclassifiedRosterRows = defaultDirections
	return resolved, nil
}

func NormalizeDirections(
	directions []personscope.Direction,
) ([]personscope.Direction, bool, error) {
	if len(directions) == 0 {
		return []personscope.Direction{
			personscope.FromPerson, personscope.ToPerson, personscope.Group,
		}, true, nil
	}
	selected := make(map[personscope.Direction]struct{}, len(directions))
	for _, raw := range directions {
		direction := personscope.Direction(strings.ToLower(strings.TrimSpace(string(raw))))
		switch direction {
		case personscope.FromPerson, personscope.ToPerson, personscope.Group:
			selected[direction] = struct{}{}
		default:
			return nil, false, fmt.Errorf("unknown person file direction %q", raw)
		}
	}
	result := make([]personscope.Direction, 0, len(selected))
	for _, direction := range []personscope.Direction{
		personscope.FromPerson, personscope.ToPerson, personscope.Group,
	} {
		if _, ok := selected[direction]; ok {
			result = append(result, direction)
		}
	}
	return result, false, nil
}

func normalizeParticipantIDs(ids []int64) ([]int64, error) {
	result := slices.Clone(ids)
	for _, id := range result {
		if id <= 0 {
			return nil, errors.New("resolved person scope contains an invalid participant ID")
		}
	}
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) == 0 {
		return nil, ErrEmptyPopulation
	}
	return result, nil
}
