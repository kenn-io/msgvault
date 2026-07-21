// Package tasklinks implements provider-neutral task metadata links without
// making the disposable local cache authoritative.
package tasklinks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/taskclient"
)

const (
	MailLinksKey          = "mail_links"
	MaxSnapshotFieldBytes = 512
)

var ErrUnsafeMailLinks = errors.New("mail_links metadata cannot be preserved safely")

type MessageIdentity struct {
	ArchiveUID       string
	ArchiveRevision  string
	MessageID        int64
	ConversationID   int64
	Subject          string
	From             string
	SentAt           time.Time
	SourceType       string
	SourceIdentifier string
	SourceMessageID  string
}

type MailLink struct {
	MessageID        int64  `json:"message_id"`
	ConversationID   int64  `json:"conversation_id,omitempty"`
	Subject          string `json:"subject,omitempty"`
	From             string `json:"from,omitempty"`
	SentAt           string `json:"sent_at,omitempty"`
	AddedAt          string `json:"added_at"`
	ArchiveUID       string `json:"archive_uid,omitempty"`
	SourceType       string `json:"source_type,omitempty"`
	SourceIdentifier string `json:"source_identifier,omitempty"`
	SourceMessageID  string `json:"source_message_id,omitempty"`
}

type MatchKind string

const (
	MatchSource  MatchKind = "source"
	MatchArchive MatchKind = "archive"
	MatchLegacy  MatchKind = "legacy"
)

type Match struct {
	Link                        MailLink
	Kind                        MatchKind
	RecoveredFromAnotherArchive bool
}

func NewMailLink(identity MessageIdentity, addedAt time.Time) MailLink {
	return MailLink{
		MessageID: identity.MessageID, ConversationID: identity.ConversationID,
		Subject: snapshot(identity.Subject), From: snapshot(identity.From),
		SentAt: identity.SentAt.UTC().Format(time.RFC3339), AddedAt: addedAt.UTC().Format(time.RFC3339),
		ArchiveUID: snapshot(identity.ArchiveUID), SourceType: snapshot(identity.SourceType),
		SourceIdentifier: snapshot(identity.SourceIdentifier), SourceMessageID: snapshot(identity.SourceMessageID),
	}
}

func snapshot(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	if len(value) <= MaxSnapshotFieldBytes {
		return value
	}
	value = value[:MaxSnapshotFieldBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func Resolve(links []MailLink, identity MessageIdentity) []Match {
	for _, mode := range []MatchKind{MatchSource, MatchArchive, MatchLegacy} {
		matches := make([]Match, 0)
		for _, link := range links {
			matched := false
			switch mode {
			case MatchSource:
				matched = completeSource(link.SourceType, link.SourceIdentifier, link.SourceMessageID) &&
					link.SourceType == identity.SourceType && link.SourceIdentifier == identity.SourceIdentifier && link.SourceMessageID == identity.SourceMessageID
			case MatchArchive:
				matched = link.ArchiveUID != "" && link.ArchiveUID == identity.ArchiveUID && link.MessageID == identity.MessageID
			case MatchLegacy:
				matched = link.ArchiveUID == "" && !completeSource(link.SourceType, link.SourceIdentifier, link.SourceMessageID) && link.MessageID == identity.MessageID
			}
			if matched {
				matches = append(matches, Match{Link: link, Kind: mode, RecoveredFromAnotherArchive: mode == MatchSource && link.ArchiveUID != "" && link.ArchiveUID != identity.ArchiveUID})
			}
		}
		if len(matches) > 0 {
			return matches
		}
	}
	return nil
}

func matchesAnySupportedIdentity(link MailLink, identity MessageIdentity) bool {
	source := completeSource(link.SourceType, link.SourceIdentifier, link.SourceMessageID) &&
		link.SourceType == identity.SourceType &&
		link.SourceIdentifier == identity.SourceIdentifier &&
		link.SourceMessageID == identity.SourceMessageID
	archive := link.ArchiveUID != "" && link.ArchiveUID == identity.ArchiveUID && link.MessageID == identity.MessageID
	legacy := link.ArchiveUID == "" &&
		!completeSource(link.SourceType, link.SourceIdentifier, link.SourceMessageID) &&
		link.MessageID == identity.MessageID
	return source || archive || legacy
}

func completeSource(sourceType, identifier, messageID string) bool {
	return sourceType != "" && identifier != "" && messageID != ""
}

func MailLinks(metadata map[string]any) []MailLink {
	_, links, err := rawMailLinks(metadata)
	if err != nil {
		return nil
	}
	return links
}

// indexMailLinks tolerates individual incompatible elements so a remote task
// with one valid link cannot disappear from the disposable reverse index. The
// boolean reports that the result is incomplete. Mutation paths intentionally
// continue to use rawMailLinks and fail closed on the same input.
func indexMailLinks(metadata map[string]any) ([]MailLink, bool) {
	raw, ok := metadata[MailLinksKey]
	if !ok {
		return nil, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, true
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(data, &elements); err != nil {
		return nil, true
	}
	links := make([]MailLink, 0, len(elements))
	incompatible := false
	for _, element := range elements {
		var object map[string]any
		if err := json.Unmarshal(element, &object); err != nil || object == nil {
			incompatible = true
			continue
		}
		var link MailLink
		if err := json.Unmarshal(element, &link); err != nil {
			incompatible = true
			continue
		}
		links = append(links, link)
	}
	return links, incompatible
}

func rawMailLinks(metadata map[string]any) ([]any, []MailLink, error) {
	raw, ok := metadata[MailLinksKey]
	if !ok {
		return []any{}, []MailLink{}, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: encode existing value: %w", ErrUnsafeMailLinks, err)
	}
	var elements []any
	if err := json.Unmarshal(data, &elements); err != nil {
		return nil, nil, fmt.Errorf("%w: expected an array: %w", ErrUnsafeMailLinks, err)
	}
	links := make([]MailLink, 0, len(elements))
	for index, element := range elements {
		object, ok := element.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%w: element %d is not an object", ErrUnsafeMailLinks, index)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: encode element %d: %w", ErrUnsafeMailLinks, index, err)
		}
		var link MailLink
		if err := json.Unmarshal(encoded, &link); err != nil {
			return nil, nil, fmt.Errorf("%w: decode element %d: %w", ErrUnsafeMailLinks, index, err)
		}
		links = append(links, link)
	}
	return elements, links, nil
}

func cloneMetadata(metadata map[string]any) map[string]any {
	clone := make(map[string]any, len(metadata)+1)
	maps.Copy(clone, metadata)
	return clone
}

func MetadataWithLink(metadata map[string]any, link MailLink) map[string]any {
	result, err := metadataWithLink(metadata, link)
	if err != nil {
		return cloneMetadata(metadata)
	}
	return result
}

func metadataWithLink(metadata map[string]any, link MailLink) (map[string]any, error) {
	result := cloneMetadata(metadata)
	raw, links, err := rawMailLinks(result)
	if err != nil {
		return nil, err
	}
	identity := MessageIdentity{ArchiveUID: link.ArchiveUID, MessageID: link.MessageID, SourceType: link.SourceType, SourceIdentifier: link.SourceIdentifier, SourceMessageID: link.SourceMessageID}
	if len(Resolve(links, identity)) == 0 {
		encoded, err := json.Marshal(link)
		if err != nil {
			return nil, fmt.Errorf("encode new mail link: %w", err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			return nil, fmt.Errorf("normalize new mail link: %w", err)
		}
		raw = append(raw, object)
	}
	result[MailLinksKey] = raw
	return result, nil
}

func metadataWithoutLink(metadata map[string]any, identity MessageIdentity) (map[string]any, error) {
	result := cloneMetadata(metadata)
	raw, links, err := rawMailLinks(result)
	if err != nil {
		return nil, err
	}
	kept := make([]any, 0, len(raw))
	for index, link := range links {
		if !matchesAnySupportedIdentity(link, identity) {
			kept = append(kept, raw[index])
		}
	}
	result[MailLinksKey] = kept
	return result, nil
}

type MutationClient interface {
	CreateTask(ctx context.Context, project, idempotencyKey string, create taskclient.TaskCreate) (taskclient.Task, error)
	GetTask(ctx context.Context, project, taskID string) (taskclient.Task, error)
	MutateMetadata(ctx context.Context, project, taskID, revision string, metadata map[string]any) (taskclient.Task, error)
}

type Service struct {
	Client  MutationClient
	Project string
	Now     func() time.Time
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s Service) Create(ctx context.Context, key string, create taskclient.TaskCreate, identity MessageIdentity) (taskclient.Task, error) {
	metadata, err := metadataWithLink(create.Metadata, NewMailLink(identity, s.now()))
	if err != nil {
		return taskclient.Task{}, err
	}
	create.Metadata = metadata
	return s.Client.CreateTask(ctx, s.Project, key, create)
}

func (s Service) Link(ctx context.Context, taskID string, identity MessageIdentity) (taskclient.Task, error) {
	return s.mutate(ctx, taskID, identity, false)
}

func (s Service) Unlink(ctx context.Context, taskID string, identity MessageIdentity) (taskclient.Task, error) {
	return s.mutate(ctx, taskID, identity, true)
}

func (s Service) mutate(ctx context.Context, taskID string, identity MessageIdentity, unlink bool) (taskclient.Task, error) {
	for attempt := range 2 {
		task, err := s.Client.GetTask(ctx, s.Project, taskID)
		if err != nil {
			return taskclient.Task{}, err
		}
		_, existingLinks, err := rawMailLinks(task.Metadata)
		if err != nil {
			return taskclient.Task{}, err
		}
		if !unlink && len(Resolve(existingLinks, identity)) > 0 {
			return task, nil
		}
		metadata, err := metadataWithLink(task.Metadata, NewMailLink(identity, s.now()))
		if unlink {
			metadata, err = metadataWithoutLink(task.Metadata, identity)
		}
		if err != nil {
			return taskclient.Task{}, err
		}
		updated, err := s.Client.MutateMetadata(ctx, s.Project, taskID, task.Revision, metadata)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, taskclient.ErrConflict) || attempt == 1 {
			return taskclient.Task{}, err
		}
	}
	panic("unreachable")
}
