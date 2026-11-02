package api

import (
	"context"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

type senderCeilingStore interface {
	SenderClassCeilings(ctx context.Context, participantIDs, personIDs, allowedSourceIDs []int64) (store.SenderClassCeilings, error)
}

func (s *Server) readSenderCeilings(ctx context.Context, participants, persons []int64) store.SenderClassCeilings {
	reader, ok := s.store.(senderCeilingStore)
	if !ok {
		return store.SenderClassCeilings{}
	}
	result, err := reader.SenderClassCeilings(ctx, participants, persons, sourcescope.IDs(ctx))
	if err != nil {
		s.logger.Warn("sender ceiling evidence unavailable", "error", err)
		return store.SenderClassCeilings{}
	}
	return result
}

func (s *Server) fillParticipantSenderCeiling(ctx context.Context, person *query.PersonSummary, requestedID int64) {
	if person != nil {
		result := s.readSenderCeilings(ctx, []int64{requestedID}, nil)
		person.SenderCeilingReadout = store.SenderCeiling(result.Participants[requestedID])
	}
}

func (s *Server) fillParticipantSenderCeilings(ctx context.Context, people []query.PersonSummary) {
	for start := 0; start < len(people); start += 500 {
		batch := people[start:min(start+500, len(people))]
		ids := make([]int64, len(batch))
		for i := range batch {
			ids[i] = batch[i].ID
		}
		result := s.readSenderCeilings(ctx, ids, nil)
		for i := range batch {
			batch[i].SenderCeilingReadout = store.SenderCeiling(result.Participants[batch[i].ID])
		}
	}
}

func (s *Server) fillPersonSenderCeiling(ctx context.Context, person *store.Person) {
	if person != nil {
		result := s.readSenderCeilings(ctx, nil, []int64{person.ID})
		person.SenderCeilingReadout = store.SenderCeiling(result.Persons[person.ID])
	}
}

func (s *Server) fillPersonSenderCeilings(ctx context.Context, people []store.Person) {
	for start := 0; start < len(people); start += 500 {
		batch := people[start:min(start+500, len(people))]
		ids := make([]int64, len(batch))
		for i := range batch {
			ids[i] = batch[i].ID
		}
		result := s.readSenderCeilings(ctx, nil, ids)
		for i := range batch {
			batch[i].SenderCeilingReadout = store.SenderCeiling(result.Persons[batch[i].ID])
		}
	}
}

func (s *Server) fillDirectorySenderCeilings(ctx context.Context, people []store.DirectoryPersonSummary) {
	ids := make([]int64, len(people))
	for i := range people {
		ids[i] = people[i].ID
	}
	result := s.readSenderCeilings(ctx, nil, ids)
	for i := range people {
		people[i].SenderCeilingReadout = store.SenderCeiling(result.Persons[people[i].ID])
	}
}
