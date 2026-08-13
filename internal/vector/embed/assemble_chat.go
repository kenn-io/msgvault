package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector"
)

const (
	chatDocumentPolicyVersion = "v3"
	defaultChatGap            = 30 * time.Minute
	// Stable global-ID blocks make updates, deletes, and moves repairable from
	// the durable journal without retaining an unbounded chat day.
	chatScopeMaxMessages = 32
	// Bounds one chat message's body: body_text is truncated to this many
	// characters in SQL (prefix-stable), and the preprocessed canonical text
	// is capped to this many runes in chatMembers — which is what actually
	// bounds the HTML path, since body_html must ship whole to keep
	// HTML-to-text canonicalization identical to search-time hydration.
	chatMessageBodyMaxChars = 16 * 1024
)

// ChatWindowAssembler resolves complete desired Beeper windows from persisted
// conversation/range scopes. It never needs the changed seed message to remain
// live, so old delete and move scopes can repair their former documents.
type ChatWindowAssembler struct {
	Policy AssemblyPolicy
}

type chatMember struct {
	row    AssemblyMessage
	chunks []ChunkSpan
	cut    bool
}

// AssembleScopes reads each bounded source scope from the supplied snapshot,
// sessionizes it deterministically, and returns every affected document once.
func (a ChatWindowAssembler) AssembleScopes(ctx context.Context, snapshot SourceSnapshot, scopes []AffectedScope) ([]Document, error) {
	normalized := make([]AffectedScope, 0, len(scopes))
	for _, scope := range scopes {
		if scope.ConversationID == 0 {
			continue
		}
		scope.MessageID = 0
		scope.Kind = contextualChatMessageType
		normalized = append(normalized, scope)
	}
	normalized = deduplicateScopes(normalized)

	documents := make(map[string]Document)
	latestBlocks := make(map[int64]int64)
	for _, scope := range normalized {
		rows, err := snapshot.ChatMessages(ctx, scope)
		if err != nil {
			return nil, err
		}
		members := a.members(rows)
		if len(members) == 0 {
			continue
		}
		conversation, found, err := snapshot.Conversation(ctx, scope.ConversationID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		latestBlock, known := latestBlocks[scope.ConversationID]
		if !known {
			latestID, latestFound, err := snapshot.latestChatMessageID(ctx, scope.ConversationID)
			if err != nil {
				return nil, err
			}
			if latestFound {
				latestBlock = chatBlockStart(latestID)
			}
			latestBlocks[scope.ConversationID] = latestBlock
		}
		includeMutableMetadata := scope.MessageIDStart == latestBlock
		for _, window := range a.sessionize(conversation, members, includeMutableMetadata) {
			doc := a.document(snapshot.SourceSequence(), scopeKeyForSelector(scope), conversation, window,
				includeMutableMetadata)
			documents[doc.Key] = doc
		}
	}

	result := make([]Document, 0, len(documents))
	for _, document := range documents {
		result = append(result, document)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

func (a ChatWindowAssembler) members(rows []AssemblyMessage) []chatMember {
	ordered := append([]AssemblyMessage(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].SentAt.Equal(ordered[j].SentAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].SentAt.Before(ordered[j].SentAt)
	})
	members := make([]chatMember, 0, len(ordered))
	for _, row := range ordered {
		if row.MessageType != contextualChatMessageType || a.shouldSkip(row) {
			continue
		}
		body, bodyCut := Preprocess("", row.Body, 0, a.Policy.Preprocess)
		if strings.TrimSpace(body) == "" {
			continue
		}
		// Cap the canonical text, not the raw input: the capped body stays an
		// exact rune-prefix of what search-time hydration recomputes from the
		// full source, so stored offsets keep slicing identical excerpt text.
		body, capCut := canonicalRunePrefix(body, chatMessageBodyMaxChars)
		spans, tailCut := ChunkText(body, a.Policy.MaxChunkRunes, 0, maxSpansPerMessage)
		members = append(members, chatMember{
			row: row, chunks: spans, cut: row.BodyTruncated || bodyCut || capCut || tailCut,
		})
	}
	return members
}

func (a ChatWindowAssembler) shouldSkip(row AssemblyMessage) bool {
	return a.Policy.SkipMessage != nil && a.Policy.SkipMessage(row)
}

// canonicalRunePrefix caps s at maxRunes runes, cutting on a rune boundary.
func canonicalRunePrefix(s string, maxRunes int) (string, bool) {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s, false
	}
	return string([]rune(s)[:maxRunes]), true
}

func (a ChatWindowAssembler) sessionize(
	conversation AssemblyConversation, members []chatMember, includeMutableMetadata bool,
) [][]chatMember {
	if len(members) == 0 {
		return nil
	}
	gap := a.Policy.ChatGap
	if gap <= 0 {
		gap = defaultChatGap
	}
	windows := make([][]chatMember, 0)
	current := make([]chatMember, 0)
	currentBytes := 0
	for _, member := range members {
		header := chatHeader(conversation, member.row.SentAt, includeMutableMetadata)
		memberBytes := chatMemberEmbeddingBytes(header, member, includeMutableMetadata)
		split := false
		if len(current) != 0 {
			previous := current[len(current)-1]
			split = chatDay(previous.row.SentAt) != chatDay(member.row.SentAt)
			if !split && !previous.row.SentAt.IsZero() && !member.row.SentAt.IsZero() {
				split = member.row.SentAt.Sub(previous.row.SentAt) > gap
			}
			if !split && a.Policy.MaxDocumentUTF8Bytes > 0 &&
				currentBytes+memberBytes > a.Policy.MaxDocumentUTF8Bytes {
				split = true
			}
		}
		if split {
			windows = append(windows, current)
			current = make([]chatMember, 0)
			currentBytes = 0
		}
		current = append(current, member)
		currentBytes += memberBytes
	}
	if len(current) != 0 {
		windows = append(windows, current)
	}
	return windows
}

func (a ChatWindowAssembler) document(
	sequence int64, scopeKey string, conversation AssemblyConversation, members []chatMember,
	includeMutableMetadata bool,
) Document {
	anchor := members[0].row
	header := chatHeader(conversation, anchor.SentAt, includeMutableMetadata)
	chunks := make([]OwnedChunk, 0)
	versions := make([]SourceVersion, 0, len(members))
	for _, member := range members {
		versions = append(versions, SourceVersion{
			MessageID: member.row.ID, LastModified: member.row.LastModified,
		})
		for i, span := range member.chunks {
			chunks = append(chunks, OwnedChunk{
				MessageID: member.row.ID, ChunkIndex: i,
				Text:            contextualChatText(header, member.row, span.Text, includeMutableMetadata),
				SourceCharStart: span.CharStart, SourceCharEnd: span.CharEnd,
				SourceBasis: vector.SourceBasisBody, Truncated: member.cut,
			})
		}
	}
	chunks = limitOwnedChunksToRequest(chunks, a.Policy.MaxDocumentUTF8Bytes, defaultVoyageRequestLimits.MaxChunks)
	key := "chat:" + strconv.FormatInt(conversation.ID, 10) + ":" +
		chatDocumentPolicyVersion + ":" + strconv.FormatInt(anchor.ID, 10)
	metadataVersion := conversation.MetadataVersion
	if !includeMutableMetadata {
		metadataVersion = MetadataVersion{}
	}
	return Document{
		Key: key, Kind: "beeper-window", ScopeKey: scopeKey,
		Revision:        chatDocumentRevision(key, header, metadataVersion, versions, chunks),
		SourceSequence:  sequence,
		Versions:        versions,
		MetadataVersion: metadataVersion,
		Chunks:          chunks,
	}
}

func chatHeader(conversation AssemblyConversation, sentAt time.Time, includeMutableMetadata bool) string {
	if !includeMutableMetadata {
		return "Conversation: historical chat\nDate: " + chatDay(sentAt)
	}
	participants := make([]string, 0, len(conversation.Participants))
	for _, participant := range conversation.Participants {
		name := strings.TrimSpace(participant.DisplayName)
		if name != "" {
			participants = append(participants, name)
		}
	}
	participantText := "Unknown participants"
	if len(participants) != 0 {
		participantText = strings.Join(participants, ", ")
	}
	title := strings.TrimSpace(conversation.Title)
	if title == "" {
		title = participantText
	}
	return "Conversation: " + title + "\nParticipants: " + participantText + "\nDate: " + chatDay(sentAt)
}

func contextualChatText(
	header string, row AssemblyMessage, source string, includeMutableMetadata bool,
) string {
	sender := "Unknown sender"
	if includeMutableMetadata {
		if display := strings.TrimSpace(row.SenderDisplay); display != "" {
			sender = display
		}
	} else {
		sender = "Unknown participant"
		if row.SenderID != 0 {
			sender = "Participant " + strconv.FormatInt(row.SenderID, 10)
		}
	}
	timestamp := "unknown time"
	if !row.SentAt.IsZero() {
		timestamp = row.SentAt.UTC().Format("15:04")
	}
	return header + "\n\n" + sender + " (" + timestamp + "): " + source
}

func chatMemberEmbeddingBytes(header string, member chatMember, includeMutableMetadata bool) int {
	total := 0
	for _, span := range member.chunks {
		total += len(contextualChatText(header, member.row, span.Text, includeMutableMetadata)) + voyagePromptReserveUTF8BytesPerChunk
	}
	return total
}

func chatDay(timestamp time.Time) string {
	if timestamp.IsZero() {
		return "undated"
	}
	return timestamp.UTC().Format("2006-01-02")
}

func chatDocumentRevision(
	key, header string,
	metadata MetadataVersion,
	versions []SourceVersion,
	chunks []OwnedChunk,
) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00", key, header,
		metadata.ConversationID, metadata.Digest)
	for _, version := range versions {
		_, _ = fmt.Fprintf(h, "%d\x00", version.MessageID)
	}
	for _, chunk := range chunks {
		_, _ = fmt.Fprintf(h, "%d\x00%d\x00%d\x00%d\x00%d\x00%t\x00%s\x00",
			chunk.MessageID, chunk.ChunkIndex, chunk.SourceCharStart, chunk.SourceCharEnd,
			chunk.SourceBasis, chunk.Truncated, chunk.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}
