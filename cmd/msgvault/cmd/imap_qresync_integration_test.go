package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

type scriptedRFC7162Message struct {
	UID       imapapi.UID
	MessageID string
	Subject   string
	Flags     []imapapi.Flag
	ModSeq    uint64
}

type scriptedRFC7162Mailbox struct {
	Name              string
	Attrs             []imapapi.MailboxAttr
	UIDValidity       uint32
	UIDNext           uint32
	HighestModSeq     uint64
	SelectUIDValidity *uint32
	SelectUIDNext     *uint32
	Messages          []scriptedRFC7162Message
	ChangedUIDs       []imapapi.UID
	VanishedUIDs      []imapapi.UID
	StatusFailure     bool
}

type scriptedRFC7162Snapshot struct {
	Capabilities   string
	Mailboxes      []scriptedRFC7162Mailbox
	QresyncFailure string
	Fallback       *scriptedRFC7162Snapshot
}

func (s scriptedRFC7162Snapshot) clone() scriptedRFC7162Snapshot {
	cloned := s
	cloned.Mailboxes = make([]scriptedRFC7162Mailbox, len(s.Mailboxes))
	for i, mailbox := range s.Mailboxes {
		mailbox.Attrs = append([]imapapi.MailboxAttr(nil), mailbox.Attrs...)
		mailbox.ChangedUIDs = append([]imapapi.UID(nil), mailbox.ChangedUIDs...)
		mailbox.VanishedUIDs = append([]imapapi.UID(nil), mailbox.VanishedUIDs...)
		messages := make([]scriptedRFC7162Message, len(mailbox.Messages))
		for j, message := range mailbox.Messages {
			message.Flags = append([]imapapi.Flag(nil), message.Flags...)
			messages[j] = message
		}
		mailbox.Messages = messages
		cloned.Mailboxes[i] = mailbox
	}
	if s.Fallback != nil {
		fallback := s.Fallback.clone()
		cloned.Fallback = &fallback
	}
	return cloned
}

type scriptedRFC7162Server struct {
	mu          sync.Mutex
	snapshot    scriptedRFC7162Snapshot
	commands    map[int][]string
	connections int
}

func startScriptedRFC7162Server(
	t *testing.T,
	snapshot scriptedRFC7162Snapshot,
) (string, *scriptedRFC7162Server) {
	t.Helper()
	require.NotEmpty(t, snapshot.Capabilities)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	server := &scriptedRFC7162Server{
		snapshot: snapshot.clone(),
		commands: make(map[int][]string),
	}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			server.mu.Lock()
			server.connections++
			connID := server.connections
			connSnapshot := server.snapshot.clone()
			server.mu.Unlock()
			go serveScriptedRFC7162Conn(conn, connID, connSnapshot, server)
		}
	}()
	return listener.Addr().String(), server
}

func (s *scriptedRFC7162Server) setSnapshot(snapshot scriptedRFC7162Snapshot) {
	s.mu.Lock()
	s.snapshot = snapshot.clone()
	s.mu.Unlock()
}

func (s *scriptedRFC7162Server) record(connID int, command string) {
	s.mu.Lock()
	s.commands[connID] = append(s.commands[connID], command)
	s.mu.Unlock()
}

func (s *scriptedRFC7162Server) commandsFor(connID int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.ToUpper(strings.Join(s.commands[connID], "\n"))
}

func (s *scriptedRFC7162Server) connectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

func serveScriptedRFC7162Conn(
	conn net.Conn,
	connID int,
	snapshot scriptedRFC7162Snapshot,
	server *scriptedRFC7162Server,
) {
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "* OK scripted RFC 7162 server ready\r\n")
	reader := bufio.NewReader(conn)
	selectedMailbox := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		server.record(connID, line)
		tag, command, _ := strings.Cut(line, " ")
		upper := strings.ToUpper(command)
		switch {
		case strings.HasPrefix(upper, "CAPABILITY"):
			_, _ = fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK CAPABILITY completed\r\n",
				snapshot.Capabilities, tag)
		case strings.HasPrefix(upper, "LOGIN"):
			_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
		case strings.HasPrefix(upper, "ENABLE"):
			_, _ = fmt.Fprintf(conn, "* ENABLED QRESYNC\r\n%s OK ENABLE completed\r\n", tag)
		case strings.HasPrefix(upper, "LIST"):
			writeScriptedRFC7162List(conn, snapshot.Mailboxes)
			_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
		case strings.HasPrefix(upper, "STATUS"):
			mailbox, ok := scriptedRFC7162MailboxByName(
				snapshot.Mailboxes, parseScriptedMailbox(command, "STATUS"))
			if !ok && strings.Contains(upper, "(MESSAGES)") {
				_, _ = fmt.Fprintf(conn,
					"* STATUS \"INBOX\" (MESSAGES 0)\r\n%s OK STATUS completed\r\n", tag)
				continue
			}
			if !ok || mailbox.StatusFailure && !strings.Contains(upper, "(MESSAGES)") {
				_, _ = fmt.Fprintf(conn, "%s NO STATUS unavailable\r\n", tag)
				continue
			}
			_, _ = fmt.Fprintf(conn,
				"* STATUS %s (MESSAGES %d UIDNEXT %d UIDVALIDITY %d HIGHESTMODSEQ %d)\r\n%s OK STATUS completed\r\n",
				quoteScriptedMailbox(mailbox.Name), len(mailbox.Messages), mailbox.UIDNext,
				mailbox.UIDValidity, mailbox.HighestModSeq, tag)
		case strings.HasPrefix(upper, "SELECT"):
			mailbox, ok := scriptedRFC7162MailboxByName(
				snapshot.Mailboxes, parseScriptedMailbox(command, "SELECT"))
			if !ok {
				_, _ = fmt.Fprintf(conn, "%s NO mailbox missing\r\n", tag)
				continue
			}
			selectedMailbox = mailbox.Name
			writeScriptedRFC7162Select(conn, tag, mailbox)
			if snapshot.Fallback != nil &&
				(mailbox.SelectUIDValidity != nil || mailbox.SelectUIDNext != nil) {
				server.setSnapshot(*snapshot.Fallback)
			}
		case strings.HasPrefix(upper, "UID SEARCH"):
			mailbox, ok := scriptedRFC7162MailboxByName(snapshot.Mailboxes, selectedMailbox)
			if !ok {
				_, _ = fmt.Fprintf(conn, "%s BAD no selected mailbox\r\n", tag)
				continue
			}
			uids := scriptedRFC7162MessageUIDs(mailbox.Messages)
			_, _ = fmt.Fprintf(conn, "* SEARCH%s\r\n%s OK UID SEARCH completed\r\n",
				formatScriptedRFC7162UIDs(uids), tag)
		case strings.HasPrefix(upper, "UID FETCH"):
			mailbox, ok := scriptedRFC7162MailboxByName(snapshot.Mailboxes, selectedMailbox)
			if !ok {
				_, _ = fmt.Fprintf(conn, "%s BAD no selected mailbox\r\n", tag)
				continue
			}
			if strings.Contains(upper, "CHANGEDSINCE") {
				if snapshot.QresyncFailure != "" {
					if snapshot.Fallback != nil {
						server.setSnapshot(*snapshot.Fallback)
					}
					_, _ = fmt.Fprintf(conn, "%s %s scripted QRESYNC failure\r\n",
						tag, strings.ToUpper(snapshot.QresyncFailure))
					continue
				}
				writeScriptedRFC7162Delta(conn, mailbox)
			} else {
				writeScriptedRFC7162Fetch(conn, command, mailbox)
			}
			_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
		case strings.HasPrefix(upper, "LOGOUT"):
			_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
			return
		default:
			_, _ = fmt.Fprintf(conn, "%s BAD unsupported scripted command\r\n", tag)
		}
	}
}

func quoteScriptedMailbox(mailbox string) string {
	return strconv.Quote(mailbox)
}

func parseScriptedMailbox(command, commandName string) string {
	rest := strings.TrimSpace(command[len(commandName):])
	if strings.HasPrefix(rest, "\"") {
		if value, err := strconv.Unquote(rest[:strings.LastIndex(rest, "\"")+1]); err == nil {
			return value
		}
	}
	mailbox, _, _ := strings.Cut(rest, " ")
	return mailbox
}

func scriptedRFC7162MailboxByName(
	mailboxes []scriptedRFC7162Mailbox,
	name string,
) (scriptedRFC7162Mailbox, bool) {
	for _, mailbox := range mailboxes {
		if mailbox.Name == name {
			return mailbox, true
		}
	}
	return scriptedRFC7162Mailbox{}, false
}

func writeScriptedRFC7162List(w io.Writer, mailboxes []scriptedRFC7162Mailbox) {
	for _, mailbox := range mailboxes {
		attrs := make([]string, len(mailbox.Attrs))
		for i, attr := range mailbox.Attrs {
			attrs[i] = string(attr)
		}
		_, _ = fmt.Fprintf(w, "* LIST (%s) \"/\" %s\r\n",
			strings.Join(attrs, " "), quoteScriptedMailbox(mailbox.Name))
	}
}

func writeScriptedRFC7162Select(
	w io.Writer,
	tag string,
	mailbox scriptedRFC7162Mailbox,
) {
	uidValidity := mailbox.UIDValidity
	if mailbox.SelectUIDValidity != nil {
		uidValidity = *mailbox.SelectUIDValidity
	}
	uidNext := mailbox.UIDNext
	if mailbox.SelectUIDNext != nil {
		uidNext = *mailbox.SelectUIDNext
	}
	_, _ = fmt.Fprintf(w,
		"* FLAGS (\\Seen \\Flagged)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY %d]\r\n* OK [UIDNEXT %d]\r\n* OK [HIGHESTMODSEQ %d]\r\n%s OK [READ-WRITE] SELECT completed\r\n",
		len(mailbox.Messages), uidValidity, uidNext,
		mailbox.HighestModSeq, tag)
}

func scriptedUint32(value uint32) *uint32 {
	return new(value)
}

func scriptedRFC7162MessageUIDs(messages []scriptedRFC7162Message) []imapapi.UID {
	uids := make([]imapapi.UID, len(messages))
	for i, message := range messages {
		uids[i] = message.UID
	}
	slices.Sort(uids)
	return uids
}

func formatScriptedRFC7162UIDs(uids []imapapi.UID) string {
	var result strings.Builder
	for _, uid := range uids {
		result.WriteByte(' ')
		result.WriteString(strconv.FormatUint(uint64(uid), 10))
	}
	return result.String()
}

func formatScriptedRFC7162Flags(flags []imapapi.Flag) string {
	values := make([]string, len(flags))
	for i, flag := range flags {
		values[i] = string(flag)
	}
	return strings.Join(values, " ")
}

func writeScriptedRFC7162Delta(w io.Writer, mailbox scriptedRFC7162Mailbox) {
	if len(mailbox.VanishedUIDs) > 0 {
		_, _ = fmt.Fprintf(w, "* VANISHED (EARLIER) %s\r\n",
			imapapi.UIDSetNum(mailbox.VanishedUIDs...).String())
	}
	for _, uid := range mailbox.ChangedUIDs {
		message, ok := scriptedRFC7162MessageByUID(mailbox.Messages, uid)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "* %d FETCH (UID %d FLAGS (%s) MODSEQ (%d))\r\n",
			scriptedRFC7162Sequence(mailbox.Messages, uid), uid,
			formatScriptedRFC7162Flags(message.Flags), message.ModSeq)
	}
}

func scriptedRFC7162MessageByUID(
	messages []scriptedRFC7162Message,
	uid imapapi.UID,
) (scriptedRFC7162Message, bool) {
	for _, message := range messages {
		if message.UID == uid {
			return message, true
		}
	}
	return scriptedRFC7162Message{}, false
}

func scriptedRFC7162Sequence(messages []scriptedRFC7162Message, uid imapapi.UID) int {
	uids := scriptedRFC7162MessageUIDs(messages)
	return slices.Index(uids, uid) + 1
}

func parseScriptedRFC7162UIDSet(command string) []imapapi.UID {
	rest := strings.TrimSpace(command[len("UID FETCH"):])
	setText, _, _ := strings.Cut(rest, " ")
	var uids []imapapi.UID
	for item := range strings.SplitSeq(setText, ",") {
		parts := strings.Split(item, ":")
		start, err := strconv.ParseUint(parts[0], 10, 32)
		if err != nil {
			continue
		}
		end := start
		if len(parts) == 2 && parts[1] != "*" {
			end, err = strconv.ParseUint(parts[1], 10, 32)
			if err != nil {
				continue
			}
		}
		for uid := start; uid <= end; uid++ {
			uids = append(uids, imapapi.UID(uid))
		}
	}
	return uids
}

func writeScriptedRFC7162Fetch(
	w io.Writer,
	command string,
	mailbox scriptedRFC7162Mailbox,
) {
	headerOnly := strings.Contains(strings.ToUpper(command), "HEADER.FIELDS")
	for _, uid := range parseScriptedRFC7162UIDSet(command) {
		message, ok := scriptedRFC7162MessageByUID(mailbox.Messages, uid)
		if !ok {
			continue
		}
		sequence := scriptedRFC7162Sequence(mailbox.Messages, uid)
		if headerOnly {
			body := "\r\n"
			if message.MessageID != "" {
				body = fmt.Sprintf("Message-ID: <%s>\r\n\r\n", message.MessageID)
			}
			_, _ = fmt.Fprintf(w,
				"* %d FETCH (UID %d FLAGS (%s) BODY[HEADER.FIELDS (MESSAGE-ID)] {%d}\r\n%s)\r\n",
				sequence, uid, formatScriptedRFC7162Flags(message.Flags), len(body), body)
			continue
		}
		raw := scriptedRFC7162RawMessage(message)
		_, _ = fmt.Fprintf(w,
			"* %d FETCH (UID %d FLAGS (%s) INTERNALDATE \"01-Jan-2024 00:00:00 +0000\" RFC822.SIZE %d BODY[] {%d}\r\n%s)\r\n",
			sequence, uid, formatScriptedRFC7162Flags(message.Flags), len(raw), len(raw), raw)
	}
}

func scriptedRFC7162RawMessage(message scriptedRFC7162Message) string {
	subject := message.Subject
	if subject == "" {
		subject = "Synthetic message"
	}
	messageIDHeader := ""
	if message.MessageID != "" {
		messageIDHeader = fmt.Sprintf("Message-ID: <%s>\r\n", message.MessageID)
	}
	return fmt.Sprintf(
		"From: sender@example.test\r\nTo: recipient@example.test\r\nDate: Mon, 1 Jan 2024 00:00:00 +0000\r\n%sSubject: %s\r\n\r\nSynthetic body.\r\n",
		messageIDHeader, subject)
}

func newScriptedRFC7162Client(
	t *testing.T,
	addr string,
	opts ...imap.Option,
) *imap.Client {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := imap.NewClient(&imap.Config{
		Host: host, Port: port, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, opts...)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func runScriptedRFC7162Sync(
	t *testing.T,
	st *store.Store,
	identifier string,
	addr string,
) (*imap.Client, *store.Source, error) {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(t, err)
	client := newScriptedRFC7162Client(t, addr, imapFolderStateOptions(st, source, false)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	summary, err := newMessageSyncer(client, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).
		Full(t.Context(), identifier)
	if err != nil {
		return client, source, err
	}
	if summary.Errors != 0 {
		return client, source, fmt.Errorf(
			"scripted IMAP sync completed with %d errors", summary.Errors)
	}
	return client, source, saveIMAPFolderStates(
		context.Background(), st, source, client, summary, options.Limit)
}

func requireScriptedRFC7162Sync(
	t *testing.T,
	st *store.Store,
	identifier string,
	addr string,
) (*imap.Client, *store.Source) {
	t.Helper()
	client, source, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(t, err)
	return client, source
}

func scriptedRFC7162Capabilities() string {
	return "IMAP4rev1 ENABLE QRESYNC CONDSTORE SPECIAL-USE"
}

func newScriptedRFC7162Message(uid imapapi.UID, messageID string, flags ...imapapi.Flag) scriptedRFC7162Message {
	return scriptedRFC7162Message{
		UID: uid, MessageID: messageID, Flags: flags, ModSeq: 2,
	}
}

func scriptedRFC7162Inbox(
	uidValidity uint32,
	uidNext uint32,
	modSeq uint64,
	messages ...scriptedRFC7162Message,
) scriptedRFC7162Mailbox {
	return scriptedRFC7162Mailbox{
		Name: "INBOX", UIDValidity: uidValidity, UIDNext: uidNext,
		HighestModSeq: modSeq, Messages: messages,
	}
}

func queryScriptedRFC7162Memberships(
	t *testing.T,
	st *store.Store,
	sourceID int64,
) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT mailbox, uid, flags FROM imap_message_memberships
		WHERE source_id = ? ORDER BY mailbox, uid
	`), sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var memberships []string
	for rows.Next() {
		var mailbox, flags string
		var uid uint32
		require.NoError(t, rows.Scan(&mailbox, &uid, &flags))
		var decodedFlags []string
		require.NoError(t, json.Unmarshal([]byte(flags), &decodedFlags))
		canonicalFlags, err := json.Marshal(decodedFlags)
		require.NoError(t, err)
		memberships = append(memberships, fmt.Sprintf("%s|%d|%s", mailbox, uid, canonicalFlags))
	}
	require.NoError(t, rows.Err())
	slices.Sort(memberships)
	return memberships
}

func queryScriptedRFC7162LabelsBySourceMessageID(
	t *testing.T,
	st *store.Store,
	sourceID int64,
) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT messages.source_message_id, labels.source_label_id
		FROM messages
		JOIN message_labels ON message_labels.message_id = messages.id
		JOIN labels ON labels.id = message_labels.label_id
		WHERE messages.source_id = ?
	`), sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var labels []string
	for rows.Next() {
		var sourceMessageID, label string
		require.NoError(t, rows.Scan(&sourceMessageID, &label))
		labels = append(labels, sourceMessageID+"|"+label)
	}
	require.NoError(t, rows.Err())
	slices.Sort(labels)
	return labels
}

func installScriptedRFC7162ApplyFailureTrigger(t *testing.T, st *store.Store) {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`
			CREATE FUNCTION fail_scripted_imap_apply() RETURNS trigger AS $$
			BEGIN
				RAISE EXCEPTION 'scripted IMAP apply failure';
			END;
			$$ LANGUAGE plpgsql
		`)
		require.NoError(t, err)
		_, err = st.DB().Exec(`
			CREATE TRIGGER fail_scripted_imap_apply
			BEFORE UPDATE ON imap_folder_state
			FOR EACH ROW EXECUTE FUNCTION fail_scripted_imap_apply()
		`)
		require.NoError(t, err)
		return
	}
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_scripted_imap_apply
		BEFORE UPDATE ON imap_folder_state
		BEGIN
			SELECT RAISE(ABORT, 'scripted IMAP apply failure');
		END
	`)
	require.NoError(t, err)
}

func queryScriptedRFC7162MessageState(
	t *testing.T,
	st *store.Store,
	sourceID int64,
	messageID string,
) (sourceMessageID string, deleted bool, isRead bool) {
	t.Helper()
	var deletedAt sql.NullTime
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT source_message_id, deleted_from_source_at, is_read
		FROM messages WHERE source_id = ? AND rfc822_message_id = ?
	`), sourceID, "<"+messageID+">").Scan(&sourceMessageID, &deletedAt, &isRead))
	return sourceMessageID, deletedAt.Valid, isRead
}

func queryScriptedRFC7162MessageLabels(
	t *testing.T,
	st *store.Store,
	sourceID int64,
	messageID string,
) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT l.source_label_id
		FROM messages m
		JOIN message_labels ml ON ml.message_id = m.id
		JOIN labels l ON l.id = ml.label_id
		WHERE m.source_id = ? AND m.rfc822_message_id = ?
		ORDER BY l.source_label_id
	`), sourceID, "<"+messageID+">")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var labels []string
	for rows.Next() {
		var label string
		require.NoError(t, rows.Scan(&label))
		labels = append(labels, label)
	}
	require.NoError(t, rows.Err())
	return labels
}

func TestIMAPQresyncEndToEndAppend(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	baseline := scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
			77, 2, 1, newScriptedRFC7162Message(1, "existing@example.test"))},
	}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://append@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{{
			Name: "INBOX", UIDValidity: 77, UIDNext: 3, HighestModSeq: 2,
			Messages: []scriptedRFC7162Message{
				newScriptedRFC7162Message(1, "existing@example.test"),
				newScriptedRFC7162Message(2, "appended@example.test", imapapi.FlagSeen),
			},
			ChangedUIDs: []imapapi.UID{2},
		}},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())

	known, err := st.GetIMAPKnownUIDs(source.ID)
	requirements.NoError(err)
	assertions.Equal(map[string][]uint32{"INBOX": {1, 2}}, known)
	assertions.Equal([]string{
		"INBOX|1|[]", "INBOX|2|[\"\\\\Seen\"]",
	}, queryScriptedRFC7162Memberships(t, st, source.ID))
	commands := server.commandsFor(2)
	assertions.Contains(commands, "ENABLE QRESYNC")
	assertions.Contains(commands, "SELECT INBOX (CONDSTORE)")
	assertions.Contains(commands, "CHANGEDSINCE 1 VANISHED")
	assertions.NotContains(commands, "UID SEARCH")
}

func TestIMAPQresyncEndToEndRetiresChangedMailboxTopology(t *testing.T) {
	t.Run("deleted mailbox removes cursor and tombstones last membership", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		baseline := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{
				scriptedRFC7162Inbox(77, 1, 1),
				{Name: "Retired", UIDValidity: 88, UIDNext: 2, HighestModSeq: 1,
					Messages: []scriptedRFC7162Message{
						newScriptedRFC7162Message(1, "retired-topology@example.test"),
					}},
			},
		}
		addr, server := startScriptedRFC7162Server(t, baseline)
		st := testutil.NewTestStore(t)
		const identifier = "imap://retired-topology@example.test"
		first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(first.Close())

		server.setSnapshot(scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes:    []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(77, 1, 1)},
		})
		second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(second.Close())

		states, err := st.GetIMAPFolderStates(source.ID)
		requirements.NoError(err)
		assertions.Equal([]store.IMAPFolderState{{
			Mailbox: "INBOX", UIDValidity: 77, UIDNext: 1, HighestModSeq: 1,
		}}, states)
		assertions.Empty(queryScriptedRFC7162Memberships(t, st, source.ID))
		_, deleted, _ := queryScriptedRFC7162MessageState(
			t, st, source.ID, "retired-topology@example.test")
		assertions.True(deleted)
		assertions.NotContains(server.commandsFor(2), "CHANGEDSINCE")
		assertions.Contains(server.commandsFor(2), "LIST")
		assertions.Empty(server.commandsFor(3), "an ineligible QRESYNC baseline issues no command, so nothing needs discarding")
	})

	t.Run("renamed mailbox replaces cursor and label", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		message := newScriptedRFC7162Message(1, "renamed-topology@example.test")
		baseline := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{
				scriptedRFC7162Inbox(77, 1, 1),
				{Name: "Old", UIDValidity: 88, UIDNext: 2, HighestModSeq: 1,
					Messages: []scriptedRFC7162Message{message}},
			},
		}
		addr, server := startScriptedRFC7162Server(t, baseline)
		st := testutil.NewTestStore(t)
		const identifier = "imap://renamed-topology@example.test"
		first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(first.Close())

		server.setSnapshot(scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{
				scriptedRFC7162Inbox(77, 1, 1),
				{Name: "New", UIDValidity: 99, UIDNext: 2, HighestModSeq: 1,
					Messages: []scriptedRFC7162Message{message}},
			},
		})
		second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(second.Close())

		states, err := st.GetIMAPFolderStates(source.ID)
		requirements.NoError(err)
		requirements.Len(states, 2)
		stateNames := []string{states[0].Mailbox, states[1].Mailbox}
		slices.Sort(stateNames)
		assertions.Equal([]string{"INBOX", "New"}, stateNames)
		assertions.Equal([]string{"New"}, queryScriptedRFC7162MessageLabels(
			t, st, source.ID, "renamed-topology@example.test"))
		_, deleted, _ := queryScriptedRFC7162MessageState(
			t, st, source.ID, "renamed-topology@example.test")
		assertions.False(deleted)
		assertions.NotContains(server.commandsFor(2), "CHANGEDSINCE")
		assertions.Contains(server.commandsFor(2), "UID SEARCH")
		assertions.Empty(server.commandsFor(3), "an ineligible QRESYNC baseline issues no command, so nothing needs discarding")
	})

	t.Run("zero current mailboxes removes the final cursor", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		baseline := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
				77, 2, 1, newScriptedRFC7162Message(1, "zero-topology@example.test"))},
		}
		addr, server := startScriptedRFC7162Server(t, baseline)
		st := testutil.NewTestStore(t)
		const identifier = "imap://zero-topology@example.test"
		first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(first.Close())

		server.setSnapshot(scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes:    []scriptedRFC7162Mailbox{},
		})
		second := newScriptedRFC7162Client(
			t, addr, imapFolderStateOptions(st, source, false)...)
		options := msgsync.DefaultOptions()
		options.SourceType = sourceTypeIMAP
		options.NoResume = true
		summary, err := newMessageSyncer(second, st, options).
			WithLogger(slog.New(slog.DiscardHandler)).
			Full(t.Context(), identifier)
		requirements.NoError(err)
		deltas := second.ObservedMailboxDeltas()
		assertions.NotNil(deltas,
			"an authoritative empty topology must remain distinct from suppressed publication")
		assertions.Empty(deltas)
		requirements.NoError(saveIMAPFolderStates(
			context.Background(), st, source, second, summary, options.Limit))
		requirements.NoError(second.Close())

		states, err := st.GetIMAPFolderStates(source.ID)
		requirements.NoError(err)
		assertions.Empty(states)
		assertions.Empty(queryScriptedRFC7162Memberships(t, st, source.ID))
		_, deleted, _ := queryScriptedRFC7162MessageState(
			t, st, source.ID, "zero-topology@example.test")
		assertions.True(deleted)
		assertions.NotContains(server.commandsFor(2), "CHANGEDSINCE")
		assertions.Contains(server.commandsFor(2), "LIST")
		assertions.Empty(server.commandsFor(3), "an ineligible QRESYNC baseline issues no command, so nothing needs discarding")
	})
}

func TestIMAPQresyncEndToEndFlagOnlyChangePreservesLocalReadState(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	baseline := scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
			77, 2, 1, newScriptedRFC7162Message(1, "flags@example.test"))},
	}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://flags@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE messages SET is_read = ? WHERE source_id = ? AND source_message_id = ?
	`), false, source.ID, "INBOX|1")
	requirements.NoError(err)

	changed := scriptedRFC7162Inbox(
		77, 2, 2,
		newScriptedRFC7162Message(1, "flags@example.test", imapapi.FlagSeen, imapapi.FlagFlagged),
	)
	changed.ChangedUIDs = []imapapi.UID{1}
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{changed},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())

	assertions.Equal([]string{
		"INBOX|1|[\"\\\\Flagged\",\"\\\\Seen\"]",
	}, queryScriptedRFC7162Memberships(t, st, source.ID))
	_, deleted, isRead := queryScriptedRFC7162MessageState(
		t, st, source.ID, "flags@example.test")
	assertions.False(deleted)
	assertions.False(isRead, "provider flags must not overwrite local read state")
	assertions.NotContains(server.commandsFor(2), "UID SEARCH")
}

func TestIMAPQresyncEndToEndLimitedRunPreservesOverlappingMailboxLabels(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	message := newScriptedRFC7162Message(1, "limited-overlap@example.test")
	baseline := scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{
			scriptedRFC7162Inbox(77, 2, 1, message),
			{Name: "Archive", UIDValidity: 88, UIDNext: 2, HighestModSeq: 1,
				Messages: []scriptedRFC7162Message{message}},
		},
	}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://limited-overlap@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	changed := newScriptedRFC7162Message(
		1, "limited-overlap@example.test", imapapi.FlagSeen)
	inbox := scriptedRFC7162Inbox(77, 2, 2, changed)
	inbox.ChangedUIDs = []imapapi.UID{1}
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{
			inbox,
			{Name: "Archive", UIDValidity: 88, UIDNext: 2, HighestModSeq: 1,
				Messages: []scriptedRFC7162Message{message}},
		},
	})
	limitedClient := newScriptedRFC7162Client(
		t, addr, imapFolderStateOptions(st, source, false)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	options.Limit = 1
	summary, err := newMessageSyncer(limitedClient, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).Full(t.Context(), identifier)
	requirements.NoError(err)
	requirements.NoError(saveIMAPFolderStates(
		context.Background(), st, source, limitedClient, summary, options.Limit))
	requirements.NoError(limitedClient.Close())

	assertions.Equal([]string{"Archive", "INBOX"}, queryScriptedRFC7162MessageLabels(
		t, st, source.ID, "limited-overlap@example.test"))
	assertions.NotContains(server.commandsFor(2), "CHANGEDSINCE")
	assertions.Contains(server.commandsFor(2), "UID SEARCH")
	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	for _, state := range states {
		assertions.Equal(uint64(1), state.HighestModSeq,
			"a limited run must not publish mailbox cursors")
	}
}

func TestIMAPQresyncEndToEndMoveAndFinalExpunge(t *testing.T) {
	t.Run("move keeps the message live under its new membership", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		baseline := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{
				scriptedRFC7162Inbox(77, 2, 1, newScriptedRFC7162Message(1, "move@example.test")),
				{Name: "Archive", UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
			},
		}
		addr, server := startScriptedRFC7162Server(t, baseline)
		st := testutil.NewTestStore(t)
		const identifier = "imap://move@example.test"
		first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(first.Close())

		server.setSnapshot(scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "INBOX", UIDValidity: 77, UIDNext: 2, HighestModSeq: 2,
					VanishedUIDs: []imapapi.UID{1}},
				{Name: "Archive", UIDValidity: 88, UIDNext: 2, HighestModSeq: 2,
					Messages:    []scriptedRFC7162Message{newScriptedRFC7162Message(1, "move@example.test")},
					ChangedUIDs: []imapapi.UID{1}},
			},
		})
		second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(second.Close())

		assertions.Equal([]string{"Archive|1|[]"},
			queryScriptedRFC7162Memberships(t, st, source.ID))
		_, deleted, _ := queryScriptedRFC7162MessageState(t, st, source.ID, "move@example.test")
		assertions.False(deleted)
		assertions.NotContains(server.commandsFor(2), "UID SEARCH")
	})

	t.Run("final expunge tombstones the archived message", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		baseline := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
				77, 2, 1, newScriptedRFC7162Message(1, "expunge@example.test"))},
		}
		addr, server := startScriptedRFC7162Server(t, baseline)
		st := testutil.NewTestStore(t)
		const identifier = "imap://expunge@example.test"
		first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(first.Close())

		empty := scriptedRFC7162Inbox(77, 2, 2)
		empty.VanishedUIDs = []imapapi.UID{1}
		server.setSnapshot(scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{empty},
		})
		second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(second.Close())

		assertions.Empty(queryScriptedRFC7162Memberships(t, st, source.ID))
		_, deleted, _ := queryScriptedRFC7162MessageState(t, st, source.ID, "expunge@example.test")
		assertions.True(deleted)
		assertions.Contains(server.commandsFor(2), "VANISHED")
	})
}

func TestIMAPQresyncEndToEndReplaysAfterFailedApplication(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	baseline := scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(77, 1, 1)},
	}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://replay@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	changed := scriptedRFC7162Inbox(
		77, 2, 2, newScriptedRFC7162Message(1, "replay@example.test"))
	changed.ChangedUIDs = []imapapi.UID{1}
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{changed},
	})
	failedClient := newScriptedRFC7162Client(
		t, addr, imapFolderStateOptions(st, source, false)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	summary, err := newMessageSyncer(failedClient, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).Full(t.Context(), identifier)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM messages WHERE source_id = ? AND source_message_id = ?
	`), source.ID, "INBOX|1")
	requirements.NoError(err)
	err = saveIMAPFolderStates(context.Background(), st, source, failedClient, summary, 0)
	requirements.Error(err)
	requirements.NoError(failedClient.Close())

	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	assertions.Equal(uint64(1), states[0].HighestModSeq,
		"failed application must not publish the new cursor")
	replayed, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(replayed.Close())
	known, err := st.GetIMAPKnownUIDs(source.ID)
	requirements.NoError(err)
	assertions.Equal(map[string][]uint32{"INBOX": {1}}, known)
	assertions.Contains(server.commandsFor(3), "CHANGEDSINCE 1 VANISHED")
}

func TestIMAPQresyncEndToEndFailedMoveApplyPreservesBaselineLabels(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	baseline := scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{
			scriptedRFC7162Inbox(77, 2, 1, newScriptedRFC7162Message(1, "failed-move@example.test")),
			{Name: "Archive", UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
		},
	}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://failed-move@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{
			{Name: "INBOX", UIDValidity: 77, UIDNext: 2, HighestModSeq: 2,
				VanishedUIDs: []imapapi.UID{1}},
			{Name: "Archive", UIDValidity: 88, UIDNext: 2, HighestModSeq: 2,
				Messages:    []scriptedRFC7162Message{newScriptedRFC7162Message(1, "failed-move@example.test")},
				ChangedUIDs: []imapapi.UID{1}},
		},
	})
	failedClient := newScriptedRFC7162Client(
		t, addr, imapFolderStateOptions(st, source, false)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	summary, err := newMessageSyncer(failedClient, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).Full(t.Context(), identifier)
	requirements.NoError(err)
	installScriptedRFC7162ApplyFailureTrigger(t, st)
	err = saveIMAPFolderStates(context.Background(), st, source, failedClient, summary, 0)
	requirements.ErrorContains(err, "scripted IMAP apply failure")
	requirements.NoError(failedClient.Close())

	assertions.Equal([]string{"INBOX"},
		queryScriptedRFC7162MessageLabels(t, st, source.ID, "failed-move@example.test"))
	assertions.Equal([]string{"INBOX|1|[]"},
		queryScriptedRFC7162Memberships(t, st, source.ID))
	_, deleted, _ := queryScriptedRFC7162MessageState(
		t, st, source.ID, "failed-move@example.test")
	assertions.False(deleted)
	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	for _, state := range states {
		assertions.Equal(uint64(1), state.HighestModSeq)
	}
}

func TestIMAPQresyncEndToEndUIDValidityReset(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	baseline := scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
			77, 2, 1, newScriptedRFC7162Message(1, "old-epoch@example.test"))},
	}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://uidvalidity@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
			99, 2, 1, newScriptedRFC7162Message(1, "new-epoch@example.test"))},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())

	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	assertions.Equal(uint32(99), states[0].UIDValidity)
	assertions.Equal([]string{"INBOX|1|[]"},
		queryScriptedRFC7162Memberships(t, st, source.ID))
	newSourceID, deleted, _ := queryScriptedRFC7162MessageState(
		t, st, source.ID, "new-epoch@example.test")
	assertions.Equal("INBOX|1", newSourceID)
	assertions.False(deleted)
	assertions.NotContains(server.commandsFor(2), "CHANGEDSINCE")
	assertions.Contains(server.commandsFor(2), "UID SEARCH")
	assertions.Empty(server.commandsFor(3), "an ineligible QRESYNC baseline issues no command, so nothing needs discarding")
}

func TestIMAPQresyncEndToEndConservativeFallbacks(t *testing.T) {
	t.Run("unsupported capability uses full enumeration", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		baseline := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes:    []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(77, 1, 1)},
		}
		addr, server := startScriptedRFC7162Server(t, baseline)
		st := testutil.NewTestStore(t)
		const identifier = "imap://unsupported@example.test"
		first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(first.Close())

		changed := scriptedRFC7162Inbox(
			77, 2, 0, newScriptedRFC7162Message(1, "fallback@example.test"))
		server.setSnapshot(scriptedRFC7162Snapshot{
			Capabilities: "IMAP4rev1", Mailboxes: []scriptedRFC7162Mailbox{changed},
		})
		second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(second.Close())

		known, err := st.GetIMAPKnownUIDs(source.ID)
		requirements.NoError(err)
		assertions.Equal(map[string][]uint32{"INBOX": {1}}, known)
		assertions.NotContains(server.commandsFor(2), "ENABLE QRESYNC")
		assertions.Contains(server.commandsFor(2), "UID SEARCH")
		assertions.Equal(2, server.connectionCount(),
			"a server without QRESYNC must not cost a redial on every sync")
	})

	for _, response := range []string{"BAD", "NO"} {
		t.Run("QRESYNC "+response+" reconnects before full enumeration", func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			baseline := scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes:    []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(77, 1, 1)},
			}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			identifier := "imap://qresync-" + strings.ToLower(response) + "@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			requirements.NoError(first.Close())

			changed := scriptedRFC7162Inbox(
				77, 2, 2, newScriptedRFC7162Message(1, "fresh-fallback@example.test"))
			changed.ChangedUIDs = []imapapi.UID{1}
			fallback := scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
					77, 3, 3,
					newScriptedRFC7162Message(1, "fresh-fallback@example.test"),
					newScriptedRFC7162Message(2, "fallback-race@example.test"),
				)},
			}
			server.setSnapshot(scriptedRFC7162Snapshot{
				Capabilities:   scriptedRFC7162Capabilities(),
				Mailboxes:      []scriptedRFC7162Mailbox{changed},
				QresyncFailure: response,
				Fallback:       &fallback,
			})
			second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
			requirements.NoError(second.Close())

			known, err := st.GetIMAPKnownUIDs(source.ID)
			requirements.NoError(err)
			assertions.Equal(map[string][]uint32{"INBOX": {1, 2}}, known)
			states, err := st.GetIMAPFolderStates(source.ID)
			requirements.NoError(err)
			assertions.Equal(uint32(3), states[0].UIDNext,
				"fallback cursor must come from the fresh connection")
			assertions.Equal(uint64(3), states[0].HighestModSeq)
			assertions.Contains(server.commandsFor(2), "CHANGEDSINCE 1 VANISHED")
			assertions.NotContains(server.commandsFor(2), "UID SEARCH")
			assertions.Contains(server.commandsFor(3), "UID SEARCH")
			assertions.GreaterOrEqual(server.connectionCount(), 3)
		})
	}

	t.Run("fresh fallback relists new special-use mailbox", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		baseline := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
				77, 2, 1, newScriptedRFC7162Message(1, "fallback-mailbox@example.test"))},
		}
		addr, server := startScriptedRFC7162Server(t, baseline)
		st := testutil.NewTestStore(t)
		const identifier = "imap://fallback-mailbox@example.test"
		first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(first.Close())

		failed := scriptedRFC7162Inbox(77, 2, 2)
		failed.VanishedUIDs = []imapapi.UID{1}
		fallback := scriptedRFC7162Snapshot{
			Capabilities: scriptedRFC7162Capabilities(),
			Mailboxes: []scriptedRFC7162Mailbox{
				scriptedRFC7162Inbox(77, 2, 2),
				{Name: "[Gmail]/All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
					UIDValidity: 99, UIDNext: 2, HighestModSeq: 1,
					Messages: []scriptedRFC7162Message{
						newScriptedRFC7162Message(1, "fallback-mailbox@example.test"),
					}},
			},
		}
		server.setSnapshot(scriptedRFC7162Snapshot{
			Capabilities:   scriptedRFC7162Capabilities(),
			Mailboxes:      []scriptedRFC7162Mailbox{failed},
			QresyncFailure: "BAD",
			Fallback:       &fallback,
		})
		second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
		requirements.NoError(second.Close())

		assertions.Equal([]string{"[Gmail]/All Mail|1|[]"},
			queryScriptedRFC7162Memberships(t, st, source.ID))
		_, deleted, _ := queryScriptedRFC7162MessageState(
			t, st, source.ID, "fallback-mailbox@example.test")
		assertions.False(deleted)
		assertions.Contains(server.commandsFor(3), "LIST")
		assertions.Contains(server.commandsFor(3), "SELECT \"[GMAIL]/ALL MAIL\"")
	})
}

func TestIMAPQresyncEndToEndUnsupportedAllDoesNotUseStaleShortcut(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	allMail := scriptedRFC7162Mailbox{
		Name: "[Gmail]/All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
		UIDValidity: 77, UIDNext: 2, HighestModSeq: 1,
		Messages: []scriptedRFC7162Message{
			newScriptedRFC7162Message(1, "unsupported-all@example.test"),
		},
	}
	inbox := scriptedRFC7162Inbox(
		88, 2, 1, newScriptedRFC7162Message(1, "unsupported-all@example.test"))
	addr, server := startScriptedRFC7162Server(t, scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	st := testutil.NewTestStore(t)
	const identifier = "imap://unsupported-all@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE messages SET is_read = ? WHERE source_id = ? AND rfc822_message_id = ?
	`), false, source.ID, "<unsupported-all@example.test>")
	requirements.NoError(err)

	allMail.Messages[0].Flags = []imapapi.Flag{imapapi.FlagSeen}
	inbox.Messages[0].Flags = []imapapi.Flag{imapapi.FlagSeen}
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: "IMAP4rev1 SPECIAL-USE",
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())

	assertions.Equal([]string{
		"INBOX|1|[\"\\\\Seen\"]", "[Gmail]/All Mail|1|[\"\\\\Seen\"]",
	}, queryScriptedRFC7162Memberships(t, st, source.ID))
	_, deleted, isRead := queryScriptedRFC7162MessageState(
		t, st, source.ID, "unsupported-all@example.test")
	assertions.False(deleted)
	assertions.False(isRead, "provider \\Seen must not overwrite local read state")
	assertions.Contains(server.commandsFor(2), "UID SEARCH")
	assertions.Empty(server.commandsFor(3), "an ineligible QRESYNC baseline issues no command, so nothing needs discarding")
}

func TestIMAPQresyncEndToEndGmailAllAvoidsMembershipScan(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	allMail := scriptedRFC7162Mailbox{
		Name: "[Gmail]/All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
		UIDValidity: 77, UIDNext: 2, HighestModSeq: 1,
		Messages: []scriptedRFC7162Message{newScriptedRFC7162Message(1, "all-existing@example.test")},
	}
	inbox := scriptedRFC7162Inbox(
		88, 2, 1, newScriptedRFC7162Message(1, "all-existing@example.test"))
	addr, server := startScriptedRFC7162Server(t, scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	st := testutil.NewTestStore(t)
	const identifier = "imap://all-mail@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	allMail.UIDNext = 3
	allMail.HighestModSeq = 2
	allMail.Messages = append(allMail.Messages,
		newScriptedRFC7162Message(2, "all-appended@example.test"))
	allMail.ChangedUIDs = []imapapi.UID{2}
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())

	known, err := st.GetIMAPKnownUIDs(source.ID)
	requirements.NoError(err)
	assertions.Equal(map[string][]uint32{
		"INBOX": {1}, "[Gmail]/All Mail": {1, 2},
	}, known)
	assertions.NotContains(server.commandsFor(2), "UID SEARCH",
		"valid global QRESYNC must not rebuild the full membership map")
	assertions.Contains(server.commandsFor(2), "CHANGEDSINCE 1 VANISHED")
}

func TestIMAPQresyncEndToEndGmailAllImportsUnidentifiedSecondaryMembership(t *testing.T) {
	for _, tt := range []struct {
		name      string
		messageID string
	}{
		{name: "missing message ID"},
		{name: "invalid message ID", messageID: "not a valid message id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			allMail := scriptedRFC7162Mailbox{
				Name: "[Gmail]/All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
				UIDValidity: 77, UIDNext: 2, HighestModSeq: 1,
				Messages: []scriptedRFC7162Message{newScriptedRFC7162Message(1, tt.messageID)},
			}
			inbox := scriptedRFC7162Inbox(
				88, 2, 1, newScriptedRFC7162Message(1, tt.messageID))
			addr, _ := startScriptedRFC7162Server(t, scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
			})
			st := testutil.NewTestStore(t)
			client, source, err := runScriptedRFC7162Sync(
				t, st, "imap://all-mail-unidentified@example.test", addr)
			requirements.NoError(err)
			requirements.NoError(client.Close())

			assertions.Equal([]string{"INBOX|1|[]", "[Gmail]/All Mail|1|[]"},
				queryScriptedRFC7162Memberships(t, st, source.ID))
			assertions.Equal([]string{
				"[Gmail]/All Mail|1|INBOX", "[Gmail]/All Mail|1|[Gmail]/All Mail",
			},
				queryScriptedRFC7162LabelsBySourceMessageID(t, st, source.ID))
		})
	}
}

func TestIMAPQresyncEndToEndGmailAllKeepsIdenticalUnidentifiedMessagesDistinct(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	allMail := scriptedRFC7162Mailbox{
		Name: "[Gmail]/All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
		UIDValidity: 77, UIDNext: 3, HighestModSeq: 1,
		Messages: []scriptedRFC7162Message{
			newScriptedRFC7162Message(1, ""),
			newScriptedRFC7162Message(2, ""),
		},
	}
	inbox := scriptedRFC7162Inbox(88, 2, 1, newScriptedRFC7162Message(1, ""))
	addr, _ := startScriptedRFC7162Server(t, scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	st := testutil.NewTestStore(t)
	client, source, err := runScriptedRFC7162Sync(
		t, st, "imap://all-mail-identical-unidentified@example.test", addr)
	requirements.NoError(err)
	requirements.NoError(client.Close())

	assertions.Equal([]string{
		"INBOX|1|[]", "[Gmail]/All Mail|1|[]", "[Gmail]/All Mail|2|[]",
	}, queryScriptedRFC7162Memberships(t, st, source.ID))
	assertions.Equal([]string{
		"INBOX|1|INBOX",
		"[Gmail]/All Mail|1|[Gmail]/All Mail",
		"[Gmail]/All Mail|2|[Gmail]/All Mail",
	}, queryScriptedRFC7162LabelsBySourceMessageID(t, st, source.ID))
}

func TestIMAPQresyncEndToEndGmailAllIncrementalUnidentifiedMembershipUsesDurableRaw(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	message := newScriptedRFC7162Message(1, "")
	allMail := scriptedRFC7162Mailbox{
		Name: "[Gmail]/All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
		UIDValidity: 77, UIDNext: 2, HighestModSeq: 1,
		Messages: []scriptedRFC7162Message{message},
	}
	inbox := scriptedRFC7162Inbox(88, 1, 1)
	addr, server := startScriptedRFC7162Server(t, scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	st := testutil.NewTestStore(t)
	const identifier = "imap://all-mail-durable-raw@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	inbox = scriptedRFC7162Inbox(88, 2, 2, message)
	inbox.ChangedUIDs = []imapapi.UID{1}
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())
	aliases, err := st.GetIMAPSourceMessageAliases(source.ID)
	requirements.NoError(err)
	assertions.Equal("[Gmail]/All Mail|1", aliases["INBOX|1"])

	inbox.HighestModSeq = 3
	inbox.Messages[0].Flags = []imapapi.Flag{imapapi.FlagFlagged}
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	third, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(third.Close())
	canonicalSourceMessageID, aliased := third.CanonicalSourceMessageID("INBOX|1")
	assertions.True(aliased)
	assertions.Equal("[Gmail]/All Mail|1", canonicalSourceMessageID)

	assertions.Equal([]string{"INBOX|1|[\"\\\\Flagged\"]", "[Gmail]/All Mail|1|[]"},
		queryScriptedRFC7162Memberships(t, st, source.ID))
	assertions.Equal([]string{
		"[Gmail]/All Mail|1|INBOX", "[Gmail]/All Mail|1|[Gmail]/All Mail",
	}, queryScriptedRFC7162LabelsBySourceMessageID(t, st, source.ID))
	var liveMessages int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND deleted_from_source_at IS NULL
	`), source.ID).Scan(&liveMessages))
	assertions.Equal(1, liveMessages, "a repeated secondary change must not revive its dedup copy")
	assertions.Contains(server.commandsFor(2), "CHANGEDSINCE 1 VANISHED")
	assertions.NotContains(server.commandsFor(2), "UID SEARCH")
}

func TestIMAPQresyncEndToEndGmailAllStatusFailureSuppressesPublication(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	allMail := scriptedRFC7162Mailbox{
		Name: "[Gmail]/All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
		UIDValidity: 77, UIDNext: 1, HighestModSeq: 1,
	}
	inbox := scriptedRFC7162Inbox(88, 1, 1)
	addr, server := startScriptedRFC7162Server(t, scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	st := testutil.NewTestStore(t)
	const identifier = "imap://all-status@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	inbox.StatusFailure = true
	allMail.HighestModSeq = 2
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes:    []scriptedRFC7162Mailbox{inbox, allMail},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())

	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	requirements.Len(states, 2)
	for _, state := range states {
		assertions.Equal(uint64(1), state.HighestModSeq)
	}
	assertions.Contains(server.commandsFor(2), "SELECT INBOX")
	assertions.Empty(server.commandsFor(3), "an ineligible QRESYNC baseline issues no command, so nothing needs discarding")
}

func TestIMAPQresyncEndToEndNoAllEmptyStatusFailureSuppressesPublication(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	baseline := scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{
			scriptedRFC7162Inbox(77, 1, 1),
			{Name: "Archive", UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
		},
	}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://no-all-status@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(first.Close())

	failedArchive := baseline.Mailboxes[1]
	failedArchive.StatusFailure = true
	server.setSnapshot(scriptedRFC7162Snapshot{
		Capabilities: scriptedRFC7162Capabilities(),
		Mailboxes: []scriptedRFC7162Mailbox{
			scriptedRFC7162Inbox(77, 1, 2),
			failedArchive,
		},
	})
	second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
	requirements.NoError(second.Close())

	states, err := st.GetIMAPFolderStates(source.ID)
	requirements.NoError(err)
	requirements.Len(states, 2)
	for _, state := range states {
		assertions.Equal(uint64(1), state.HighestModSeq,
			"one failed STATUS must suppress the entire authoritative snapshot")
	}
	assertions.NotContains(server.commandsFor(2), "CHANGEDSINCE")
	assertions.Contains(server.commandsFor(2), `SELECT "ARCHIVE"`)
	assertions.Empty(server.commandsFor(3), "an ineligible QRESYNC baseline issues no command, so nothing needs discarding")
}

func TestIMAPQresyncEndToEndZeroStatusCursorSuppressesPublication(t *testing.T) {
	tests := []struct {
		name        string
		uidValidity uint32
		uidNext     uint32
	}{
		{name: "zero UIDVALIDITY", uidValidity: 0, uidNext: 2},
		{name: "zero UIDNEXT", uidValidity: 77, uidNext: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			message := newScriptedRFC7162Message(1, "zero-status@example.test")
			baseline := scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes: []scriptedRFC7162Mailbox{
					scriptedRFC7162Inbox(77, 2, 1, message),
				},
			}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			identifier := "imap://zero-status-" + strings.ReplaceAll(tt.name, " ", "-") + "@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			requirements.NoError(first.Close())

			server.setSnapshot(scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes: []scriptedRFC7162Mailbox{{
					Name: "INBOX", UIDValidity: tt.uidValidity, UIDNext: tt.uidNext,
					HighestModSeq: 2, Messages: []scriptedRFC7162Message{message},
				}},
			})
			second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
			requirements.NoError(second.Close())

			states, err := st.GetIMAPFolderStates(source.ID)
			requirements.NoError(err)
			requirements.Len(states, 1)
			assertions.Equal(uint32(77), states[0].UIDValidity)
			assertions.Equal(uint32(2), states[0].UIDNext)
			assertions.Equal(uint64(1), states[0].HighestModSeq)
		})
	}
}

func TestIMAPQresyncEndToEndSelectCursorValidationFallsBackBeforeDeltaFetch(t *testing.T) {
	tests := []struct {
		name              string
		selectUIDValidity *uint32
		selectUIDNext     *uint32
	}{
		{name: "zero UIDVALIDITY", selectUIDValidity: scriptedUint32(0)},
		{name: "zero UIDNEXT", selectUIDNext: scriptedUint32(0)},
		{name: "regressed UIDNEXT", selectUIDNext: scriptedUint32(1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			baseline := scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
					77, 2, 1, newScriptedRFC7162Message(1, "select-cursor@example.test"))},
			}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			identifier := "imap://select-cursor-" + strings.ReplaceAll(tt.name, " ", "-") + "@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			requirements.NoError(first.Close())

			invalid := scriptedRFC7162Inbox(
				77, 2, 2, newScriptedRFC7162Message(1, "select-cursor@example.test"))
			invalid.ChangedUIDs = []imapapi.UID{1}
			invalid.SelectUIDValidity = tt.selectUIDValidity
			invalid.SelectUIDNext = tt.selectUIDNext
			fallback := scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes: []scriptedRFC7162Mailbox{scriptedRFC7162Inbox(
					77, 2, 2, newScriptedRFC7162Message(1, "select-cursor@example.test"))},
			}
			server.setSnapshot(scriptedRFC7162Snapshot{
				Capabilities: scriptedRFC7162Capabilities(),
				Mailboxes:    []scriptedRFC7162Mailbox{invalid},
				Fallback:     &fallback,
			})
			second, _ := requireScriptedRFC7162Sync(t, st, identifier, addr)
			requirements.NoError(second.Close())

			assertions.NotContains(server.commandsFor(2), "CHANGEDSINCE",
				"invalid SELECT state must be rejected before delta FETCH")
			assertions.Contains(server.commandsFor(3), "UID SEARCH",
				"invalid SELECT state must force fresh full enumeration")
			states, err := st.GetIMAPFolderStates(source.ID)
			requirements.NoError(err)
			requirements.Len(states, 1)
			assertions.Equal(uint32(77), states[0].UIDValidity)
			assertions.Equal(uint32(2), states[0].UIDNext)
			assertions.Equal(uint64(2), states[0].HighestModSeq)
		})
	}
}
