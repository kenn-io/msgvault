package meetingimport

import (
	"context"
	"log/slog"

	"go.kenn.io/msgvault/internal/apiimport"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type (
	Status = apiimport.Status
	Result = apiimport.Result
	Hooks  = apiimport.Hooks
)

const (
	StatusCreated = apiimport.StatusCreated
	StatusUpdated = apiimport.StatusUpdated
)

var ErrUnavailable = apiimport.ErrUnavailable

type Importer struct {
	base *apiimport.Importer
}

func NewImporter(s *store.Store, hooks Hooks) *Importer {
	return &Importer{base: apiimport.NewImporter(s, "meeting import", hooks)}
}

// WithLogger sets the logger used for best-effort post-import work.
func (i *Importer) WithLogger(logger *slog.Logger) *Importer {
	i.base.WithLogger(logger)
	return i
}

func (i *Importer) Import(ctx context.Context, req Request) (Result, error) {
	if i == nil || !i.base.Available() {
		return Result{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	normalized, err := req.Normalize()
	if err != nil {
		return Result{}, err
	}
	snapshot, err := BuildSnapshot(normalized)
	if err != nil {
		return Result{}, err
	}

	var organizer *meetingarchive.Person
	if snapshot.Organizer != nil {
		person := archivePerson(snapshot.SourceIdentifier, *snapshot.Organizer)
		organizer = &person
	}
	attendees := make([]meetingarchive.Person, 0, len(snapshot.Attendees))
	for _, attendee := range snapshot.Attendees {
		attendees = append(attendees, archivePerson(snapshot.SourceIdentifier, attendee))
	}
	return i.base.Import(ctx, apiimport.Item{
		Source: apiimport.Source{
			Type:         SourceType,
			Identifier:   snapshot.SourceIdentifier,
			DisplayName:  snapshot.SourceDisplayName,
			AccountEmail: snapshot.AccountEmail,
		},
		SourceMessageID: snapshot.SourceMessageID,
		// The archive derives the meeting's own attribution as it writes.
		ExcludeFromReattribution: true,
		Write: func(ctx context.Context, st *store.Store, sourceID int64) (apiimport.Write, error) {
			archived, err := meetingarchive.New(st).Upsert(ctx, meetingarchive.Snapshot{
				SourceID:             sourceID,
				AccountEmail:         snapshot.AccountEmail,
				SourceMessageID:      snapshot.SourceMessageID,
				SourceConversationID: snapshot.SourceMessageID,
				Title:                snapshot.Title,
				StartedAt:            snapshot.StartedAt,
				Body:                 snapshot.Body,
				Snippet:              snapshot.Snippet,
				Metadata:             snapshot.Metadata,
				Raw:                  snapshot.Raw,
				RawFormat:            RawFormat,
				Organizer:            organizer,
				Attendees:            attendees,
			}, meetingarchive.UpsertOptions{})
			return apiimport.Write{
				MessageID: archived.MessageID,
				Created:   archived.Created,
				Changed:   archived.Changed,
			}, err
		},
	})
}

// archivePerson maps an imported person to the archive. The anchor is scoped
// to the import source, so the same id from two sources names two people.
func archivePerson(sourceIdentifier string, person MeetingPerson) meetingarchive.Person {
	archived := meetingarchive.Person{Name: person.Name, Email: person.Email, Phone: person.Phone}
	if person.ID != "" {
		archived.Anchor = meetingarchive.Anchor("meeting-import", sourceIdentifier, person.ID)
	}
	return archived
}
