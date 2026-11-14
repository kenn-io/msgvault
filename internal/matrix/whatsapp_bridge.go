package matrix

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// A mautrix-whatsapp bridge puts every WhatsApp chat of its logins in a Matrix
// portal room. Archived as plain Matrix rooms, those messages would carry
// Matrix ghost IDs instead of phone numbers and Matrix event IDs instead of
// WhatsApp message IDs, so they would neither join the people that the native
// WhatsApp importers identify by phone nor deduplicate against them.
//
// The bridge database holds what the Matrix events lack: each event's
// WhatsApp message ID, the chat and sender JIDs, and the LID-to-phone map.
// ImportWhatsAppBridge reads it, fetches each event's content from Matrix, and
// writes the messages into the whatsapp source of the login's phone number,
// with the same conversation keys, message IDs and phone identities as the
// WhatsApp Desktop importer. A message both importers see is stored once.

const (
	// WhatsAppBridgeSyncType names the sync runs this importer records on
	// the whatsapp source of each bridge login.
	WhatsAppBridgeSyncType  = "whatsapp_matrix_bridge"
	whatsAppBridgeRawFormat = "matrix_whatsapp_json"
	whatsAppSourceType      = "whatsapp"
	// whatsAppBridgeWindow bounds an incremental run. Bridge rows older than
	// this are only revisited by a full run; edits rarely come later.
	whatsAppBridgeWindow = 7 * 24 * time.Hour
	whatsAppSnippetRunes = 100
)

// WhatsAppBridge is a read-only handle on a mautrix-whatsapp database.
type WhatsAppBridge struct {
	db *sql.DB
}

// OpenWhatsAppBridge opens the bridge's SQLite database read-only. The
// bridge owns the file and its WAL sidecars, so the archive never writes.
func OpenWhatsAppBridge(ctx context.Context, path string) (*WhatsAppBridge, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve mautrix-whatsapp database path: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("open mautrix-whatsapp database: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&_busy_timeout=5000"}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mautrix-whatsapp database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open mautrix-whatsapp database: %w", err)
	}
	return &WhatsAppBridge{db: db}, nil
}

// Close releases the database handle.
func (b *WhatsAppBridge) Close() error { return b.db.Close() }

// PortalRooms returns the Matrix room IDs of every bridged WhatsApp chat.
// The Matrix importer skips them, since ImportWhatsAppBridge archives them.
func (b *WhatsAppBridge) PortalRooms(ctx context.Context) ([]string, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT mxid FROM portal WHERE COALESCE(mxid, '') <> '' ORDER BY mxid`)
	if err != nil {
		return nil, fmt.Errorf("list WhatsApp portal rooms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, fmt.Errorf("list WhatsApp portal rooms: %w", err)
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

type bridgeLogin struct {
	ID    string // WhatsApp user part of the login, digits only
	Phone string // E.164 form of ID
}

type bridgeMessage struct {
	WAID       string // WhatsApp message ID, the native importers' source_message_id
	ChatJID    string
	SenderJID  string
	Receiver   string // login ID for direct chats, empty for groups
	EventID    id.EventID
	RoomID     id.RoomID
	PortalName string
	Timestamp  time.Time
	EditCount  int
}

// bridgeSnapshot is the bridge state one run needs, read up front.
type bridgeSnapshot struct {
	logins       []bridgeLogin
	lidPhones    map[string]string            // LID user part -> E.164 phone
	contactNames map[string]map[string]string // login ID -> E.164 phone -> name
	groupLogins  map[string][]string          // group JID -> login IDs in it
	messages     []bridgeMessage
}

func (b *WhatsAppBridge) snapshot(ctx context.Context, userID string, since time.Time) (*bridgeSnapshot, error) {
	logins, err := b.logins(ctx, userID)
	if err != nil {
		return nil, err
	}
	snap := &bridgeSnapshot{
		logins:       logins,
		lidPhones:    map[string]string{},
		contactNames: map[string]map[string]string{},
		groupLogins:  map[string][]string{},
	}
	if len(snap.logins) == 0 {
		return snap, nil
	}
	if err := b.each(ctx, `SELECT lid, pn FROM whatsmeow_lid_map`, nil, func(rows *sql.Rows) error {
		var lid, pn string
		if err := rows.Scan(&lid, &pn); err != nil {
			return err
		}
		if phone := whatsAppPhone(pn); phone != "" {
			snap.lidPhones[jidUser(lid)] = phone
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read WhatsApp LID map: %w", err)
	}
	if err := b.each(ctx, `
		SELECT COALESCE(our_jid, ''), COALESCE(their_jid, ''), COALESCE(full_name, ''),
		       COALESCE(first_name, ''), COALESCE(push_name, ''), COALESCE(business_name, '')
		FROM whatsmeow_contacts`, nil, func(rows *sql.Rows) error {
		var ours, theirs, full, first, push, business string
		if err := rows.Scan(&ours, &theirs, &full, &first, &push, &business); err != nil {
			return err
		}
		phone := snap.jidPhone(theirs)
		name := firstNonEmpty(full, first, push, business)
		if phone == "" || name == "" {
			return nil
		}
		login := jidUser(ours)
		if snap.contactNames[login] == nil {
			snap.contactNames[login] = map[string]string{}
		}
		snap.contactNames[login][phone] = name
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read WhatsApp contacts: %w", err)
	}
	if err := b.each(ctx, `
		SELECT portal_id, login_id FROM user_portal
		WHERE portal_receiver = '' AND user_mxid = ? ORDER BY portal_id, login_id`, []any{userID}, func(rows *sql.Rows) error {
		var portal, login string
		if err := rows.Scan(&portal, &login); err != nil {
			return err
		}
		snap.groupLogins[portal] = append(snap.groupLogins[portal], login)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read WhatsApp group logins: %w", err)
	}
	// One WhatsApp message can span several Matrix events (parts); the first
	// part carries the message, the native importers store one row per ID.
	if err := b.each(ctx, `
		SELECT m.id, m.mxid, m.room_id, m.room_receiver, m.timestamp, m.edit_count,
		       COALESCE(p.mxid, ''), p.name
		FROM message m
		JOIN portal p ON p.bridge_id = m.bridge_id AND p.id = m.room_id AND p.receiver = m.room_receiver
		WHERE m.timestamp >= ?
		  AND m.part_id = (
		    SELECT MIN(first.part_id) FROM message first
		    WHERE first.bridge_id = m.bridge_id AND first.room_receiver = m.room_receiver AND first.id = m.id)
		ORDER BY m.timestamp, m.rowid`, []any{since.UnixNano()}, func(rows *sql.Rows) error {
		var rawID, mxid, chat, receiver, roomID, portalName string
		var ts int64
		var edits int
		if err := rows.Scan(&rawID, &mxid, &chat, &receiver, &ts, &edits, &roomID, &portalName); err != nil {
			return err
		}
		sender, waID, ok := splitBridgeMessageID(rawID, chat)
		if !ok || roomID == "" || mxid == "" || !importableWhatsAppChat(chat) {
			return nil
		}
		snap.messages = append(snap.messages, bridgeMessage{
			WAID: waID, ChatJID: chat, SenderJID: sender, Receiver: receiver,
			EventID: id.EventID(mxid), RoomID: id.RoomID(roomID), PortalName: portalName,
			Timestamp: time.Unix(0, ts).UTC(), EditCount: edits,
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read WhatsApp bridge messages: %w", err)
	}
	return snap, nil
}

// logins returns the WhatsApp accounts that userID linked to the bridge.
func (b *WhatsAppBridge) logins(ctx context.Context, userID string) ([]bridgeLogin, error) {
	var logins []bridgeLogin
	err := b.each(ctx, `SELECT id FROM user_login WHERE user_mxid = ? ORDER BY id`, []any{userID}, func(rows *sql.Rows) error {
		var loginID string
		if err := rows.Scan(&loginID); err != nil {
			return err
		}
		if phone := whatsAppPhone(loginID); phone != "" {
			logins = append(logins, bridgeLogin{ID: loginID, Phone: phone})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read WhatsApp bridge logins: %w", err)
	}
	return logins, nil
}

// waIDForEvent maps a bridged event to its WhatsApp message ID.
func (b *WhatsAppBridge) waIDForEvent(ctx context.Context, eventID id.EventID) (string, error) {
	var rawID, chat string
	err := b.db.QueryRowContext(ctx, `SELECT id, room_id FROM message WHERE mxid = ? LIMIT 1`, eventID.String()).Scan(&rawID, &chat)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, waID, _ := splitBridgeMessageID(rawID, chat)
	return waID, nil
}

func (b *WhatsAppBridge) each(ctx context.Context, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// loginsFor returns the logins whose source receives msg: the receiver of a
// direct chat, or every login that is in a group.
func (snap *bridgeSnapshot) loginsFor(msg bridgeMessage) []string {
	if msg.Receiver != "" {
		return []string{msg.Receiver}
	}
	return snap.groupLogins[msg.ChatJID]
}

// jidPhone resolves a phone or LID JID to an E.164 phone, or "".
func (snap *bridgeSnapshot) jidPhone(jid string) string {
	user, server := splitJID(jid)
	switch server {
	case "lid":
		return snap.lidPhones[user]
	case "s.whatsapp.net", "":
		return whatsAppPhone(user)
	}
	return ""
}

// canonicalChat is the conversation key the WhatsApp Desktop importer uses:
// <digits>@s.whatsapp.net when the chat resolves to a phone, else the JID.
func (snap *bridgeSnapshot) canonicalChat(jid string) string {
	if !strings.HasSuffix(strings.ToLower(jid), "@g.us") {
		if phone := snap.jidPhone(jid); phone != "" {
			return strings.TrimPrefix(phone, "+") + "@s.whatsapp.net"
		}
	}
	return strings.ToLower(strings.TrimSpace(jid))
}

// splitBridgeMessageID splits mautrix-whatsapp's "<chat>:<sender>:<id>"
// message key. JIDs in it carry no device part, so the last colon ends the
// sender.
func splitBridgeMessageID(rawID, chat string) (sender, waID string, ok bool) {
	rest, found := strings.CutPrefix(rawID, chat+":")
	if !found {
		return "", "", false
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// splitJID returns a JID's user part without agent or device, and its server.
func splitJID(jid string) (user, server string) {
	jid = strings.ToLower(strings.TrimSpace(jid))
	user, server, _ = strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	return user, server
}

func jidUser(jid string) string {
	user, _ := splitJID(jid)
	return user
}

// whatsAppPhone turns a WhatsApp phone user part into E.164, applying the same
// rule as the native importers: 4 to 15 digits.
func whatsAppPhone(user string) string {
	user = strings.TrimPrefix(strings.TrimSpace(user), "+")
	if len(user) < 4 || len(user) > 15 {
		return ""
	}
	for _, c := range user {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return "+" + user
}

func importableWhatsAppChat(jid string) bool {
	_, server := splitJID(jid)
	return server == "s.whatsapp.net" || server == "lid" || server == "g.us"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// WhatsAppBridgeOptions configures one bridge import.
type WhatsAppBridgeOptions struct {
	// UserID is the Matrix account whose bridge logins are imported.
	UserID string
	// Full revisits every bridged message instead of the recent window.
	Full     bool
	Progress func(string)
	now      func() time.Time
}

// WhatsAppBridgeSummary counts one bridge import across its logins.
type WhatsAppBridgeSummary struct {
	Logins            int64
	LoginsBusy        int64 // skipped because another import held the source
	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	MessagesSkipped   int64
	MessagesDeleted   int64
	ErrorsCount       int64
}

type whatsAppBridgeImporter struct {
	store    *store.Store
	client   *mautrix.Client
	bridge   *WhatsAppBridge
	snap     *bridgeSnapshot
	login    bridgeLogin
	sourceID int64
	selfID   int64
	people   map[string]int64
	convs    map[string]int64
	sum      *WhatsAppBridgeSummary
}

// ImportWhatsAppBridge archives the bridged WhatsApp chats of opts.UserID
// into one whatsapp source per bridge login.
func ImportWhatsAppBridge(ctx context.Context, s *store.Store, runtime *Runtime, bridge *WhatsAppBridge, opts WhatsAppBridgeOptions) (*WhatsAppBridgeSummary, error) {
	if opts.UserID == "" {
		return nil, errors.New("matrix user ID required")
	}
	now := time.Now
	if opts.now != nil {
		now = opts.now
	}
	// A login whose source has no finished bridge run yet needs its whole
	// history; the others only need the recent window.
	full := opts.Full
	sources := map[string]*store.Source{}
	logins, err := bridge.logins(ctx, opts.UserID)
	if err != nil {
		return nil, err
	}
	for _, login := range logins {
		source, err := s.GetOrCreateSource(whatsAppSourceType, login.Phone)
		if err != nil {
			return nil, fmt.Errorf("WhatsApp source %s: %w", login.Phone, err)
		}
		sources[login.ID] = source
		if _, err := s.GetLastSuccessfulSyncByType(ctx, source.ID, WhatsAppBridgeSyncType); errors.Is(err, store.ErrSyncRunNotFound) {
			full = true
		} else if err != nil {
			return nil, err
		}
	}
	since := now().Add(-whatsAppBridgeWindow)
	if full {
		since = time.Unix(0, 0)
	}
	snap, err := bridge.snapshot(ctx, opts.UserID, since)
	if err != nil {
		return nil, err
	}
	sum := &WhatsAppBridgeSummary{}
	var failures []error
	for _, login := range snap.logins {
		source := sources[login.ID]
		if source == nil {
			// A login linked between the two reads waits for the next run.
			continue
		}
		var messages []bridgeMessage
		for _, msg := range snap.messages {
			if slices.Contains(snap.loginsFor(msg), login.ID) {
				messages = append(messages, msg)
			}
		}
		err := importWhatsAppBridgeLogin(ctx, s, runtime.Client, bridge, snap, login, source, messages, sum)
		switch {
		case errors.Is(err, store.ErrSyncAlreadyActive):
			sum.LoginsBusy++
		case err != nil:
			if ctx.Err() != nil {
				return sum, errors.Join(err, ctx.Err())
			}
			failures = append(failures, fmt.Errorf("WhatsApp %s: %w", login.Phone, err))
		default:
			sum.Logins++
		}
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("WhatsApp bridge %s: %d bridged messages", login.Phone, len(messages)))
		}
	}
	return sum, errors.Join(failures...)
}

func importWhatsAppBridgeLogin(ctx context.Context, s *store.Store, client *mautrix.Client, bridge *WhatsAppBridge, snap *bridgeSnapshot, login bridgeLogin, source *store.Source, messages []bridgeMessage, total *WhatsAppBridgeSummary) (err error) {
	syncID, err := s.StartSyncContext(ctx, source.ID, WhatsAppBridgeSyncType)
	if err != nil {
		return err
	}
	scoped := s.ScopedToSync(source.ID, syncID)
	sum := &WhatsAppBridgeSummary{}
	checkpoint := func() *store.Checkpoint {
		return &store.Checkpoint{
			MessagesProcessed: sum.MessagesProcessed, MessagesAdded: sum.MessagesAdded,
			MessagesUpdated: sum.MessagesUpdated, ErrorsCount: sum.ErrorsCount,
		}
	}
	defer func() {
		total.MessagesProcessed += sum.MessagesProcessed
		total.MessagesAdded += sum.MessagesAdded
		total.MessagesUpdated += sum.MessagesUpdated
		total.MessagesSkipped += sum.MessagesSkipped
		total.MessagesDeleted += sum.MessagesDeleted
		total.ErrorsCount += sum.ErrorsCount
		if err != nil {
			_ = scoped.FailSyncWithCheckpointContext(context.WithoutCancel(ctx), syncID, err.Error(), checkpoint())
		}
	}()
	selfID, err := scoped.EnsureParticipantByPhone(ctx, login.Phone, "", whatsAppSourceType)
	if err != nil {
		return fmt.Errorf("ensure self participant: %w", err)
	}
	imp := &whatsAppBridgeImporter{
		store: scoped, client: client, bridge: bridge, snap: snap, login: login,
		sourceID: source.ID, selfID: selfID, people: map[string]int64{login.Phone: selfID},
		convs: map[string]int64{}, sum: sum,
	}
	ids := make([]string, 0, len(messages))
	for _, msg := range messages {
		ids = append(ids, msg.WAID)
	}
	stored, err := scoped.StoredMessagesContext(ctx, source.ID, whatsAppBridgeRawFormat, ids)
	if err != nil {
		return err
	}
	for i, msg := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		sum.MessagesProcessed++
		if err := imp.importMessage(ctx, msg, stored); err != nil {
			return fmt.Errorf("message %s: %w", msg.WAID, err)
		}
		if (i+1)%200 == 0 {
			if err := scoped.UpdateSyncCheckpointContext(ctx, syncID, checkpoint()); err != nil {
				return err
			}
		}
	}
	if sum.MessagesAdded+sum.MessagesUpdated+sum.MessagesDeleted > 0 {
		if err := scoped.RecomputeConversationStatsContext(ctx, source.ID); err != nil {
			return fmt.Errorf("recompute WhatsApp conversation stats: %w", err)
		}
	}
	if err := scoped.UpdateSyncCheckpointContext(ctx, syncID, checkpoint()); err != nil {
		return err
	}
	return scoped.CompleteSyncAndPreserveSourceCursorContext(ctx, syncID, source.ID, "")
}

// whatsAppBridgeRaw is the stored raw form: the bridged event, the edit that
// supplied the current body, and the bridge keys that tie it to WhatsApp.
type whatsAppBridgeRaw struct {
	ChatJID   string       `json:"chat_jid"`
	SenderJID string       `json:"sender_jid"`
	MessageID string       `json:"message_id"`
	Event     *event.Event `json:"event"`
	Edit      *event.Event `json:"edit,omitempty"`
}

func (imp *whatsAppBridgeImporter) importMessage(ctx context.Context, msg bridgeMessage, stored map[string]store.StoredMessage) error {
	existing, exists := stored[msg.WAID]
	// A row stored by another importer (WhatsApp Desktop) stays as it is, and
	// an unedited bridged message never changes.
	if exists && (existing.Raw == nil || msg.EditCount == 0) {
		imp.sum.MessagesSkipped++
		return nil
	}
	evt, err := imp.client.GetEvent(ctx, msg.RoomID, msg.EventID)
	if errors.Is(err, mautrix.MNotFound) || errors.Is(err, mautrix.MForbidden) {
		imp.sum.MessagesSkipped++
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch Matrix event %s: %w", msg.EventID, err)
	}
	if evt.Unsigned.RedactedBecause != nil {
		if exists {
			imp.sum.MessagesDeleted++
			return imp.store.MarkMessageDeletedContext(ctx, imp.sourceID, msg.WAID)
		}
		imp.sum.MessagesSkipped++
		return nil
	}
	evt.RoomID = msg.RoomID
	if (evt.Type != event.EventMessage && evt.Type != event.EventSticker) || !parseContent(evt) {
		imp.sum.MessagesSkipped++
		return nil
	}
	content := evt.Content.AsMessage()
	body := messageBody(content)
	var edit *event.Event
	if msg.EditCount > 0 {
		edit, err = imp.latestEdit(ctx, evt)
		if err != nil {
			return err
		}
		if edit != nil {
			body = editedBody(evt, edit.Content.AsMessage().NewContent)
		}
	}
	if strings.TrimSpace(body) == "" {
		imp.sum.MessagesSkipped++
		return nil
	}

	senderPhone := imp.snap.jidPhone(msg.SenderJID)
	fromMe := senderPhone == imp.login.Phone
	convID, err := imp.conversation(ctx, msg)
	if err != nil {
		return err
	}
	var senderID sql.NullInt64
	if senderPhone != "" {
		pid, err := imp.participant(ctx, senderPhone)
		if err != nil {
			return err
		}
		senderID = sql.NullInt64{Int64: pid, Valid: true}
		if err := imp.store.EnsureConversationParticipantContext(ctx, convID, pid, "member"); err != nil {
			return fmt.Errorf("add WhatsApp sender: %w", err)
		}
	}
	raw, err := json.Marshal(whatsAppBridgeRaw{
		ChatJID: msg.ChatJID, SenderJID: msg.SenderJID, MessageID: msg.WAID, Event: evt, Edit: edit,
	}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode bridged event: %w", err)
	}
	sentAt := sql.NullTime{Time: msg.Timestamp, Valid: !msg.Timestamp.IsZero()}
	message := store.Message{
		ConversationID: convID, SourceID: imp.sourceID, SourceMessageID: msg.WAID,
		MessageType: whatsAppSourceType, SentAt: sentAt, InternalDate: sentAt,
		SenderID: senderID, IsFromMe: fromMe, Snippet: whatsAppSnippet(body),
		SizeEstimate: int64(len(body)), ArchivedAt: time.Now(),
	}
	bodyText := sql.NullString{String: body, Valid: true}
	fromAddr := senderPhone
	if fromMe {
		fromAddr = imp.login.Phone
	}
	data := &store.MessagePersistData{
		Message: &message, BodyText: bodyText, RawMIME: raw, RawFormat: whatsAppBridgeRawFormat,
		PreserveLabels: true, Edited: edit != nil,
		FTS: &store.FTSDoc{Body: body, FromAddr: fromAddr},
	}
	if exists && existing.Matches(&message, raw) {
		unchanged, err := imp.store.MessageContentMatchesContext(ctx, existing.ID, bodyText, *data.FTS)
		if err != nil {
			return err
		}
		if unchanged {
			imp.sum.MessagesSkipped++
			return nil
		}
	}
	messageID, err := imp.store.PersistMessageContext(ctx, data)
	if err != nil {
		return fmt.Errorf("persist bridged message: %w", err)
	}
	if exists {
		imp.sum.MessagesUpdated++
	} else {
		imp.sum.MessagesAdded++
	}
	return imp.linkReply(ctx, messageID, convID, content)
}

// latestEdit returns the newest valid replacement of evt, or nil.
func (imp *whatsAppBridgeImporter) latestEdit(ctx context.Context, evt *event.Event) (*event.Event, error) {
	resp, err := imp.client.GetRelations(ctx, evt.RoomID, evt.ID, &mautrix.ReqGetRelations{
		RelationType: event.RelReplace, Dir: mautrix.DirectionBackward, Limit: 50,
	})
	if err != nil {
		return nil, fmt.Errorf("fetch edits of %s: %w", evt.ID, err)
	}
	var latest *event.Event
	for _, candidate := range resp.Chunk {
		if candidate == nil || candidate.Unsigned.RedactedBecause != nil || !parseContent(candidate) {
			continue
		}
		content := candidate.Content.AsMessage()
		if content.NewContent == nil || content.RelatesTo.GetReplaceID() != evt.ID || !validReplacement(evt, candidate) {
			continue
		}
		if latest == nil || candidate.Timestamp > latest.Timestamp ||
			(candidate.Timestamp == latest.Timestamp && candidate.ID > latest.ID) {
			latest = candidate
		}
	}
	return latest, nil
}

func (imp *whatsAppBridgeImporter) conversation(ctx context.Context, msg bridgeMessage) (int64, error) {
	key := imp.snap.canonicalChat(msg.ChatJID)
	if convID, ok := imp.convs[key]; ok {
		return convID, nil
	}
	convType, title := "direct_chat", ""
	if strings.HasSuffix(key, "@g.us") {
		convType, title = "group_chat", strings.TrimSpace(msg.PortalName)
	}
	convID, err := imp.store.EnsureConversationWithTypeContext(ctx, imp.sourceID, key, convType, title)
	if err != nil {
		return 0, fmt.Errorf("ensure WhatsApp conversation: %w", err)
	}
	if err := imp.store.EnsureConversationParticipantContext(ctx, convID, imp.selfID, "member"); err != nil {
		return 0, fmt.Errorf("add self to WhatsApp conversation: %w", err)
	}
	if convType == "direct_chat" {
		if phone := imp.snap.jidPhone(msg.ChatJID); phone != "" {
			pid, err := imp.participant(ctx, phone)
			if err != nil {
				return 0, err
			}
			if err := imp.store.EnsureConversationParticipantContext(ctx, convID, pid, "member"); err != nil {
				return 0, fmt.Errorf("add WhatsApp direct participant: %w", err)
			}
		}
	}
	imp.convs[key] = convID
	return convID, nil
}

// participant returns the participant for an E.164 phone. A known contact
// name only fills a blank display name; it never replaces one.
func (imp *whatsAppBridgeImporter) participant(ctx context.Context, phone string) (int64, error) {
	if pid, ok := imp.people[phone]; ok {
		return pid, nil
	}
	pid, err := imp.store.EnsureParticipantByPhone(ctx, phone, imp.snap.contactNames[imp.login.ID][phone], whatsAppSourceType)
	if err != nil {
		return 0, fmt.Errorf("ensure WhatsApp participant: %w", err)
	}
	imp.people[phone] = pid
	return pid, nil
}

func (imp *whatsAppBridgeImporter) linkReply(ctx context.Context, messageID, convID int64, content *event.MessageEventContent) error {
	target := content.RelatesTo.GetReplyTo()
	if target == "" {
		return nil
	}
	waID, err := imp.bridge.waIDForEvent(ctx, target)
	if err != nil || waID == "" {
		return err
	}
	found, err := imp.store.MessageExistsBatchContext(ctx, imp.sourceID, []string{waID})
	if err != nil {
		return err
	}
	replyTo := found[waID]
	if replyTo == 0 {
		return nil
	}
	targetMsg, err := imp.store.GetMessageRelationTarget(ctx, replyTo)
	if err != nil || targetMsg.ConversationID != convID {
		return err
	}
	return imp.store.SetMessageReplyContext(ctx, messageID, replyTo)
}

func whatsAppSnippet(body string) sql.NullString {
	if body == "" {
		return sql.NullString{}
	}
	if utf8.RuneCountInString(body) > whatsAppSnippetRunes {
		body = string([]rune(body)[:whatsAppSnippetRunes])
	}
	return sql.NullString{String: body, Valid: true}
}
