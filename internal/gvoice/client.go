package gvoice

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textimport"
)

const defaultPageSize = 500

// Client reads from a Google Voice Takeout export and imports messages
// into the msgvault store.
type Client struct {
	takeoutDir string
	owner      ownerPhones
	identifier string // GV phone number used as source identifier
	afterDate  time.Time
	beforeDate time.Time
	limit      int
	index      []indexEntry
	indexBuilt bool
	logger     *slog.Logger
	pageSize   int

	// LRU cache for parsed HTML files (avoid re-parsing when
	// consecutive messages come from the same file)
	lastFilePath string
	lastMessages []textMessage
	lastGroupPar []string
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithAfterDate filters to messages on or after this date.
func WithAfterDate(t time.Time) ClientOption {
	return func(c *Client) { c.afterDate = t }
}

// WithBeforeDate filters to messages before this date.
func WithBeforeDate(t time.Time) ClientOption {
	return func(c *Client) { c.beforeDate = t }
}

// WithLimit sets the maximum number of messages to import.
func WithLimit(n int) ClientOption {
	return func(c *Client) { c.limit = n }
}

// WithLogger sets the logger for the client.
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) { c.logger = l }
}

// NewClient creates a Client from a Google Voice Takeout directory.
// The directory should be the "Voice" folder containing "Calls/"
// and "Phones.vcf".
func NewClient(
	takeoutDir string, opts ...ClientOption,
) (*Client, error) {
	info, err := os.Stat(takeoutDir)
	if err != nil {
		return nil, fmt.Errorf("takeout directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", takeoutDir)
	}

	callsDir := filepath.Join(takeoutDir, "Calls")
	if _, err := os.Stat(callsDir); err != nil {
		return nil, fmt.Errorf(
			"calls directory not found in %s: %w",
			takeoutDir, err,
		)
	}

	vcfPath := filepath.Join(takeoutDir, "Phones.vcf")
	vcfData, err := os.ReadFile(vcfPath)
	if err != nil {
		return nil, fmt.Errorf("read Phones.vcf: %w", err)
	}

	owner, err := parseVCF(vcfData)
	if err != nil {
		return nil, fmt.Errorf("parse Phones.vcf: %w", err)
	}

	c := &Client{
		takeoutDir: takeoutDir,
		owner:      owner,
		identifier: owner.GoogleVoice,
		logger:     slog.Default(),
		pageSize:   defaultPageSize,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c, nil
}

// Identifier returns the Google Voice phone number.
func (c *Client) Identifier() string {
	return c.identifier
}

// Close is a no-op for the Takeout client.
func (c *Client) Close() error {
	return nil
}

// Import reads all matching messages from the Takeout export and
// writes them into the msgvault store.
func (c *Client) Import(
	ctx context.Context,
	s *store.Store,
	sourceID int64,
) (*ImportSummary, error) {
	if err := c.buildIndex(); err != nil {
		return nil, fmt.Errorf("build index: %w", err)
	}

	summary := &ImportSummary{}

	// Ensure labels
	labelIDs, err := c.ensureLabels(s, sourceID)
	if err != nil {
		return nil, err
	}

	// Caches
	phoneCache := map[string]int64{} // phone → participantID
	convCache := map[string]int64{}  // threadID → conversationID
	imported := 0

	// Resolve owner participant once for all messages
	ownerID, err := c.resolveParticipant(
		s, c.owner.GoogleVoice, "",
		phoneCache, summary,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve owner: %w", err)
	}

	for _, entry := range c.index {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if c.limit > 0 && imported >= c.limit {
			break
		}

		switch entry.FileType {
		case fileTypeText, fileTypeGroup:
			n, err := c.importTextEntry(
				s, sourceID, &entry, ownerID,
				labelIDs, phoneCache, convCache, summary,
			)
			if err != nil {
				c.logger.Warn(
					"failed to import text entry",
					"id", entry.ID,
					"error", err,
				)
				summary.Skipped++
				continue
			}
			imported += n

		default:
			if err := c.importCallEntry(
				s, sourceID, &entry, ownerID,
				labelIDs, phoneCache, convCache, summary,
			); err != nil {
				c.logger.Warn(
					"failed to import call entry",
					"id", entry.ID,
					"error", err,
				)
				summary.Skipped++
				continue
			}
			imported++
		}
	}

	if err := s.RecomputeConversationStats(sourceID); err != nil {
		return summary, fmt.Errorf("recompute stats: %w", err)
	}

	return summary, nil
}

func (c *Client) ensureLabels(
	s *store.Store, sourceID int64,
) (map[string]int64, error) {
	labels := map[string]int64{}
	for _, name := range []string{
		"sms", "mms", "call_received",
		"call_placed", "call_missed", "voicemail",
	} {
		id, err := s.EnsureLabel(sourceID, name, name, "user")
		if err != nil {
			return nil, fmt.Errorf("ensure label %q: %w", name, err)
		}
		labels[name] = id
	}
	return labels, nil
}

func (c *Client) importTextEntry(
	s *store.Store,
	sourceID int64,
	entry *indexEntry,
	ownerID int64,
	labelIDs map[string]int64,
	phoneCache map[string]int64,
	convCache map[string]int64,
	summary *ImportSummary,
) (int, error) {
	messages, groupParticipants, err := c.getCachedMessages(
		entry.FilePath,
	)
	if err != nil {
		return 0, err
	}

	if entry.MessageIndex >= len(messages) {
		return 0, fmt.Errorf(
			"message index %d out of range (file has %d messages)",
			entry.MessageIndex, len(messages),
		)
	}

	msg := messages[entry.MessageIndex]

	// Resolve conversation
	convType := "direct_chat"
	title := ""
	if entry.FileType == fileTypeGroup {
		convType = "group_chat"
		title = "Group"
	}

	convID, err := c.ensureConv(
		s, sourceID, entry.ThreadID, convType, title,
		convCache, summary,
	)
	if err != nil {
		return 0, fmt.Errorf("ensure conversation: %w", err)
	}

	// Resolve sender
	senderID, err := c.resolveParticipant(
		s, msg.SenderPhone, msg.SenderName,
		phoneCache, summary,
	)
	if err != nil {
		return 0, fmt.Errorf("resolve sender: %w", err)
	}

	// For direct chats, resolve the other party (the non-Me participant).
	// For outbound messages msg.SenderPhone is the owner's number, so
	// senderID == ownerID and cannot be used as the "to" recipient.
	contactID := senderID
	if entry.FileType == fileTypeText && msg.IsMe {
		contactID = c.resolveContactID(
			s, messages, phoneCache, summary,
		)
	}

	// Ensure conversation participant
	if senderID > 0 {
		_ = s.EnsureConversationParticipant(convID, senderID, "member")
	}

	// Ensure the resolved contact is a conversation participant.
	if contactID > 0 && contactID != senderID {
		_ = s.EnsureConversationParticipant(convID, contactID, "member")
	}

	// Ensure owner as conversation participant
	if ownerID > 0 {
		_ = s.EnsureConversationParticipant(convID, ownerID, "member")
	}

	// Ensure group participants
	for _, phone := range groupParticipants {
		pid, pErr := c.resolveParticipant(
			s, phone, "", phoneCache, summary,
		)
		if pErr == nil && pid > 0 {
			_ = s.EnsureConversationParticipant(
				convID, pid, "member",
			)
		}
	}

	// Build message
	msgType := MessageTypeForFileType(entry.FileType)
	isFromMe := msg.IsMe

	senderIDNull := sql.NullInt64{}
	if senderID > 0 {
		senderIDNull = sql.NullInt64{Int64: senderID, Valid: true}
	}

	sentAt := sql.NullTime{}
	if !msg.Timestamp.IsZero() {
		sentAt = sql.NullTime{Time: msg.Timestamp, Valid: true}
	}

	hasAttachments := len(msg.Attachments) > 0

	msgID, err := s.UpsertMessage(&store.Message{
		SourceID:        sourceID,
		SourceMessageID: entry.ID,
		ConversationID:  convID,
		Snippet:         nullStr(snippet(msg.Body)),
		SentAt:          sentAt,
		MessageType:     msgType,
		SenderID:        senderIDNull,
		IsFromMe:        isFromMe,
		HasAttachments:  hasAttachments,
		SizeEstimate:    int64(len(msg.Body)),
	})
	if err != nil {
		return 0, fmt.Errorf("upsert message: %w", err)
	}

	// Store body
	if err := s.UpsertMessageBody(
		msgID,
		sql.NullString{String: msg.Body, Valid: msg.Body != ""},
		sql.NullString{},
	); err != nil {
		return 0, fmt.Errorf("upsert message body: %w", err)
	}

	// Store raw HTML
	rawData, rErr := os.ReadFile(entry.FilePath)
	if rErr == nil {
		_ = s.UpsertMessageRawWithFormat(
			msgID, rawData, "gvoice_html",
		)
	}

	// Write message_recipients
	if err := c.writeTextRecipients(
		s, msgID, msg, ownerID, senderID, contactID, entry.FileType,
		groupParticipants, phoneCache, summary,
	); err != nil {
		return 0, fmt.Errorf("write message recipients: %w", err)
	}

	// Link labels
	for _, labelName := range entry.Labels {
		if lid, ok := labelIDs[labelName]; ok {
			_ = s.LinkMessageLabel(msgID, lid)
		}
	}
	// Add mms label if has attachments
	if hasAttachments {
		if lid, ok := labelIDs["mms"]; ok {
			_ = s.LinkMessageLabel(msgID, lid)
		}
	}

	summary.MessagesImported++
	return 1, nil
}

func (c *Client) importCallEntry(
	s *store.Store,
	sourceID int64,
	entry *indexEntry,
	ownerID int64,
	labelIDs map[string]int64,
	phoneCache map[string]int64,
	convCache map[string]int64,
	summary *ImportSummary,
) error {
	f, err := os.Open(entry.FilePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	record, err := parseCallHTML(f)
	if err != nil {
		return err
	}

	// Resolve conversation
	convID, err := c.ensureConv(
		s, sourceID, entry.ThreadID, "direct_chat", "",
		convCache, summary,
	)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}

	// Resolve contact
	contactID, err := c.resolveParticipant(
		s, record.Phone, record.Name,
		phoneCache, summary,
	)
	if err != nil {
		return fmt.Errorf("resolve contact: %w", err)
	}
	if contactID > 0 {
		_ = s.EnsureConversationParticipant(
			convID, contactID, "member",
		)
	}

	// Ensure owner as conversation participant
	if ownerID > 0 {
		_ = s.EnsureConversationParticipant(
			convID, ownerID, "member",
		)
	}

	// Determine sender
	var senderID int64
	isFromMe := false
	switch record.CallType {
	case fileTypePlaced:
		senderID = ownerID
		isFromMe = true
	default:
		senderID = contactID
	}

	// Build body
	var body strings.Builder
	switch record.CallType {
	case fileTypeReceived:
		fmt.Fprintf(&body, "Received call from %s", record.Name)
	case fileTypePlaced:
		fmt.Fprintf(&body, "Placed call to %s", record.Name)
	case fileTypeMissed:
		fmt.Fprintf(&body, "Missed call from %s", record.Name)
	case fileTypeVoicemail:
		fmt.Fprintf(&body, "Voicemail from %s", record.Name)
	default:
		// fileTypeText / fileTypeGroup are not call records; leave body empty.
	}
	if record.Duration != "" {
		fmt.Fprintf(&body, " (%s)", formatDuration(record.Duration))
	}

	msgType := MessageTypeForFileType(entry.FileType)

	senderIDNull := sql.NullInt64{}
	if senderID > 0 {
		senderIDNull = sql.NullInt64{
			Int64: senderID, Valid: true,
		}
	}

	sentAt := sql.NullTime{}
	if !record.Timestamp.IsZero() {
		sentAt = sql.NullTime{
			Time: record.Timestamp, Valid: true,
		}
	}

	bodyStr := body.String()
	msgID, err := s.UpsertMessage(&store.Message{
		SourceID:        sourceID,
		SourceMessageID: entry.ID,
		ConversationID:  convID,
		Snippet:         nullStr(snippet(bodyStr)),
		SentAt:          sentAt,
		MessageType:     msgType,
		SenderID:        senderIDNull,
		IsFromMe:        isFromMe,
		SizeEstimate:    int64(len(bodyStr)),
	})
	if err != nil {
		return fmt.Errorf("upsert message: %w", err)
	}

	// Store body
	if err := s.UpsertMessageBody(
		msgID,
		sql.NullString{String: bodyStr, Valid: bodyStr != ""},
		sql.NullString{},
	); err != nil {
		return fmt.Errorf("upsert message body: %w", err)
	}

	// Store raw HTML
	rawData, rErr := os.ReadFile(entry.FilePath)
	if rErr == nil {
		_ = s.UpsertMessageRawWithFormat(
			msgID, rawData, "gvoice_html",
		)
	}

	// Write message_recipients
	if err := writeCallRecipients(
		s, msgID, ownerID, contactID, record.CallType,
	); err != nil {
		return fmt.Errorf("write message recipients: %w", err)
	}

	// Link labels
	for _, labelName := range entry.Labels {
		if lid, ok := labelIDs[labelName]; ok {
			_ = s.LinkMessageLabel(msgID, lid)
		}
	}

	summary.MessagesImported++
	return nil
}

// writeTextRecipients writes from/to rows for a text message.
// IsMe=true: from=owner, to=contact(s). IsMe=false: from=sender, to=owner.
// contactID is the other party in a direct chat (may differ from senderID
// for outbound messages where senderID == ownerID).
func (c *Client) writeTextRecipients(
	s *store.Store,
	msgID int64,
	msg textMessage,
	ownerID, senderID, contactID int64,
	ft fileType,
	groupParticipants []string,
	phoneCache map[string]int64,
	summary *ImportSummary,
) error {
	if msg.IsMe {
		// From: owner
		if ownerID > 0 {
			if err := s.ReplaceMessageRecipients(
				msgID, "from", []int64{ownerID}, nil,
			); err != nil {
				return err
			}
		}
		// To: group participants or the contact (not the sender, who is
		// the owner for outbound messages).
		// Always call Replace even with an empty slice to clear stale rows.
		toIDs := c.collectRecipientIDs(
			s, ft, contactID, groupParticipants,
			phoneCache, summary,
		)
		if err := s.ReplaceMessageRecipients(
			msgID, "to", toIDs, nil,
		); err != nil {
			return err
		}
	} else {
		// From: external sender
		if senderID > 0 {
			if err := s.ReplaceMessageRecipients(
				msgID, "from", []int64{senderID}, nil,
			); err != nil {
				return err
			}
		}
		// To: owner. Always call Replace to clear stale rows on re-import.
		var toIDs []int64
		if ownerID > 0 {
			toIDs = []int64{ownerID}
		}
		if err := s.ReplaceMessageRecipients(
			msgID, "to", toIDs, nil,
		); err != nil {
			return err
		}
	}
	return nil
}

// collectRecipientIDs returns the "to" participant IDs for an
// outgoing text message. For group chats, all group participants
// (excluding the owner). For direct chats, the contact sender ID.
func (c *Client) collectRecipientIDs(
	s *store.Store,
	ft fileType,
	contactID int64,
	groupParticipants []string,
	phoneCache map[string]int64,
	summary *ImportSummary,
) []int64 {
	if ft == fileTypeGroup && len(groupParticipants) > 0 {
		var ids []int64
		for _, phone := range groupParticipants {
			pid, err := c.resolveParticipant(
				s, phone, "", phoneCache, summary,
			)
			if err == nil && pid > 0 {
				ids = append(ids, pid)
			}
		}
		return ids
	}
	if contactID > 0 {
		return []int64{contactID}
	}
	return nil
}

// resolveContactID finds the non-Me participant in a direct chat's message
// list and returns their participant ID. Returns 0 if no non-Me sender is
// found (e.g., a file containing only outbound messages with no replies).
func (c *Client) resolveContactID(
	s *store.Store,
	messages []textMessage,
	phoneCache map[string]int64,
	summary *ImportSummary,
) int64 {
	for _, m := range messages {
		if !m.IsMe && m.SenderPhone != "" {
			pid, err := c.resolveParticipant(
				s, m.SenderPhone, m.SenderName,
				phoneCache, summary,
			)
			if err == nil && pid > 0 {
				return pid
			}
		}
	}
	return 0
}

// writeCallRecipients writes from/to rows for a call record.
// Placed calls: from=owner, to=contact.
// Received/missed/voicemail: from=contact, to=owner.
func writeCallRecipients(
	s *store.Store,
	msgID, ownerID, contactID int64,
	callType fileType,
) error {
	var fromID, toID int64
	switch callType {
	case fileTypePlaced:
		fromID = ownerID
		toID = contactID
	default:
		fromID = contactID
		toID = ownerID
	}
	if fromID > 0 {
		if err := s.ReplaceMessageRecipients(
			msgID, "from", []int64{fromID}, nil,
		); err != nil {
			return err
		}
	}
	if toID > 0 {
		if err := s.ReplaceMessageRecipients(
			msgID, "to", []int64{toID}, nil,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) ensureConv(
	s *store.Store,
	sourceID int64,
	threadID, convType, title string,
	cache map[string]int64,
	summary *ImportSummary,
) (int64, error) {
	if id, ok := cache[threadID]; ok {
		return id, nil
	}
	id, err := s.EnsureConversationWithType(
		sourceID, threadID, convType, title,
	)
	if err != nil {
		return 0, err
	}
	cache[threadID] = id
	summary.ConversationsImported++
	return id, nil
}

func (c *Client) resolveParticipant(
	s *store.Store,
	phone, displayName string,
	cache map[string]int64,
	summary *ImportSummary,
) (int64, error) {
	if phone == "" {
		return 0, nil
	}
	normalized, err := textimport.NormalizePhone(phone)
	if err != nil {
		return 0, nil //nolint:nilerr // non-normalizable phones are skipped silently
	}
	if id, ok := cache[normalized]; ok {
		return id, nil
	}
	id, err := s.EnsureParticipantByPhone(
		normalized, displayName, "google_voice",
	)
	if err != nil {
		return 0, err
	}
	cache[normalized] = id
	summary.ParticipantsResolved++
	return id, nil
}

// buildIndex walks the Calls directory, parses each HTML file, and
// builds a sorted index of all messages and call records.
func (c *Client) buildIndex() error {
	if c.indexBuilt {
		return nil
	}

	callsDir := filepath.Join(c.takeoutDir, "Calls")
	entries, err := os.ReadDir(callsDir)
	if err != nil {
		return fmt.Errorf("read Calls directory: %w", err)
	}

	c.logger.Info("building index", "files", len(entries))

	var index []indexEntry
	skipped := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		_, ft, err := classifyFile(entry.Name())
		if err != nil {
			skipped++
			continue
		}

		filePath := filepath.Join(callsDir, entry.Name())

		switch ft {
		case fileTypeText, fileTypeGroup:
			ents, err := c.indexTextFile(filePath, ft)
			if err != nil {
				c.logger.Warn(
					"failed to index text file",
					"file", entry.Name(),
					"error", err,
				)
				continue
			}
			index = append(index, ents...)

		default:
			ent, err := c.indexCallFile(filePath, ft)
			if err != nil {
				c.logger.Warn(
					"failed to index call file",
					"file", entry.Name(),
					"error", err,
				)
				continue
			}
			index = append(index, *ent)
		}
	}

	// Apply date filters
	var filtered []indexEntry
	for _, e := range index {
		if !c.afterDate.IsZero() && e.Timestamp.Before(c.afterDate) {
			continue
		}
		if !c.beforeDate.IsZero() &&
			!e.Timestamp.Before(c.beforeDate) {
			continue
		}
		filtered = append(filtered, e)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Timestamp.Before(filtered[j].Timestamp)
	})

	c.index = filtered
	c.indexBuilt = true

	c.logger.Info("index built",
		"total_entries", len(index),
		"filtered_entries", len(filtered),
		"skipped_files", skipped,
	)

	return nil
}

func (c *Client) indexTextFile(
	filePath string, ft fileType,
) ([]indexEntry, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	messages, groupParticipants, err := parseTextHTML(f)
	if err != nil {
		return nil, err
	}

	var result []indexEntry
	for i, msg := range messages {
		bodyPrefix := msg.Body
		if len(bodyPrefix) > 50 {
			bodyPrefix = bodyPrefix[:50]
		}
		id := computeMessageID(
			msg.SenderPhone,
			msg.Timestamp.Format(time.RFC3339Nano),
			bodyPrefix,
		)

		var threadID string
		if ft == fileTypeGroup {
			threadID = computeThreadID(
				fileTypeGroup,
				"", groupParticipants,
			)
		} else {
			otherPhone := msg.SenderPhone
			if msg.IsMe {
				for _, m := range messages {
					if !m.IsMe {
						otherPhone = m.SenderPhone
						break
					}
				}
			}
			threadID = computeThreadID(
				fileTypeText,
				otherPhone, nil,
			)
		}

		label := labelForFileType(ft)
		result = append(result, indexEntry{
			ID:           id,
			ThreadID:     threadID,
			FilePath:     filePath,
			MessageIndex: i,
			Timestamp:    msg.Timestamp,
			FileType:     ft,
			Labels:       []string{label},
		})
	}

	return result, nil
}

func (c *Client) indexCallFile(
	filePath string, ft fileType,
) (*indexEntry, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	record, err := parseCallHTML(f)
	if err != nil {
		return nil, err
	}

	if record.CallType != 0 {
		ft = record.CallType
	}

	id := computeMessageID(
		ft.String(), record.Phone,
		record.Timestamp.Format(time.RFC3339Nano),
	)
	threadID := computeThreadID(
		ft, record.Phone, nil,
	)
	label := labelForFileType(ft)

	return &indexEntry{
		ID:        id,
		ThreadID:  threadID,
		FilePath:  filePath,
		Timestamp: record.Timestamp,
		FileType:  ft,
		Labels:    []string{label},
	}, nil
}

// getCachedMessages returns parsed messages for a file, using a
// simple LRU cache.
func (c *Client) getCachedMessages(
	filePath string,
) ([]textMessage, []string, error) {
	if c.lastFilePath == filePath {
		return c.lastMessages, c.lastGroupPar, nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	messages, groupParticipants, err := parseTextHTML(f)
	if err != nil {
		return nil, nil, err
	}

	c.lastFilePath = filePath
	c.lastMessages = messages
	c.lastGroupPar = groupParticipants

	return messages, groupParticipants, nil
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// formatDuration converts ISO 8601 duration to human-readable format.
func formatDuration(iso string) string {
	iso = strings.TrimPrefix(iso, "PT")
	var parts []string

	if i := strings.Index(iso, "H"); i >= 0 {
		parts = append(parts, iso[:i]+"h")
		iso = iso[i+1:]
	}
	if i := strings.Index(iso, "M"); i >= 0 {
		parts = append(parts, iso[:i]+"m")
		iso = iso[i+1:]
	}
	if before, _, ok := strings.Cut(iso, "S"); ok {
		parts = append(parts, before+"s")
	}

	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}
