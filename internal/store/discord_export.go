package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DiscordExportContainer is one Discord channel, thread, or forum post in a
// bounded provider-owned export.
type DiscordExportContainer struct {
	ID       string                 `json:"id"`
	ParentID string                 `json:"parent_id"`
	Name     string                 `json:"name"`
	Kind     string                 `json:"kind"`
	Archived bool                   `json:"archived"`
	Messages []DiscordExportMessage `json:"messages"`
}

// DiscordExportMessage is the stable consumer view of one archived Discord
// message. Deleted marks source deletion; dedup-hidden rows are not exported.
type DiscordExportMessage struct {
	ID           string    `json:"id"`
	AuthorHandle string    `json:"author_handle"`
	Content      string    `json:"content"`
	SentAt       time.Time `json:"sent_at"`
	Deleted      bool      `json:"deleted"`
	URLs         []string  `json:"urls"`
}

type discordExportConversationMetadata struct {
	ParentChannelID    string `json:"parent_channel_id"`
	DiscordChannelType int    `json:"discord_channel_type"`
	Thread             *struct {
		Archived bool `json:"archived"`
	} `json:"thread"`
}

type discordExportMessageMetadata struct {
	AuthorDisplayName string `json:"author_display_name"`
}

var discordExportURLPattern = regexp.MustCompile(`https?://[^\s"'<>()]+`)

// DiscordExport returns every container for sourceID and messages whose
// effective timestamp falls in the half-open interval [start, end).
func (s *Store) DiscordExport(
	ctx context.Context, sourceID int64, start, end time.Time,
) ([]DiscordExportContainer, error) {
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT source_conversation_id, COALESCE(title, ''), COALESCE(metadata, '{}')
		FROM conversations
		WHERE source_id = ?
		ORDER BY source_conversation_id
	`), sourceID)
	if err != nil {
		return nil, fmt.Errorf("query Discord export containers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	containers := make([]DiscordExportContainer, 0)
	containerIndex := make(map[string]int)
	for rows.Next() {
		var id, name, rawMetadata string
		if err := rows.Scan(&id, &name, &rawMetadata); err != nil {
			return nil, fmt.Errorf("scan Discord export container: %w", err)
		}
		var metadata discordExportConversationMetadata
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			return nil, fmt.Errorf("decode Discord container %s metadata: %w", id, err)
		}
		kind := "channel"
		if metadata.DiscordChannelType >= 10 && metadata.DiscordChannelType <= 12 {
			kind = "thread"
		}
		archived := metadata.Thread != nil && metadata.Thread.Archived
		containerIndex[id] = len(containers)
		containers = append(containers, DiscordExportContainer{
			ID: id, ParentID: metadata.ParentChannelID, Name: name,
			Kind: kind, Archived: archived, Messages: []DiscordExportMessage{},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Discord export containers: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close Discord export containers: %w", err)
	}

	messageRows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT c.source_conversation_id,
		       m.source_message_id,
		       COALESCE(mb.body_text, ''),
		       COALESCE(m.metadata, '{}'),
		       COALESCE(m.sent_at, m.received_at, m.internal_date),
		       m.deleted_from_source_at
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_id = ?
		  AND m.deleted_at IS NULL
		  AND COALESCE(m.sent_at, m.received_at, m.internal_date) >= ?
		  AND COALESCE(m.sent_at, m.received_at, m.internal_date) < ?
		ORDER BY c.source_conversation_id,
		         COALESCE(m.sent_at, m.received_at, m.internal_date),
		         m.source_message_id
	`), sourceID, start, end)
	if err != nil {
		return nil, fmt.Errorf("query Discord export messages: %w", err)
	}
	defer func() { _ = messageRows.Close() }()

	for messageRows.Next() {
		var containerID, id, content, rawMetadata string
		var sentAt nullableTimestamp
		var deletedAt sql.NullTime
		if err := messageRows.Scan(
			&containerID, &id, &content, &rawMetadata, &sentAt, &deletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan Discord export message: %w", err)
		}
		index, ok := containerIndex[containerID]
		if !ok {
			return nil, fmt.Errorf("discord message %s references unknown container %s", id, containerID)
		}
		var metadata discordExportMessageMetadata
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			return nil, fmt.Errorf("decode Discord message %s metadata: %w", id, err)
		}
		containers[index].Messages = append(containers[index].Messages, DiscordExportMessage{
			ID:           id,
			AuthorHandle: metadata.AuthorDisplayName,
			Content:      content,
			SentAt:       sentAt.Time.UTC(),
			Deleted:      deletedAt.Valid,
			URLs:         discordExportURLs(content),
		})
	}
	if err := messageRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Discord export messages: %w", err)
	}
	return containers, nil
}

func discordExportURLs(content string) []string {
	matches := discordExportURLPattern.FindAllString(content, -1)
	if len(matches) == 0 {
		return []string{}
	}
	urls := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		match = strings.TrimRight(match, ".,;:!?]}`\"'")
		if match == "" {
			continue
		}
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		urls = append(urls, match)
	}
	return urls
}
