package inline

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	cliPageSize        = 100
	cliMaxID     int64 = 1<<53 - 1
	cliMaxStdout       = 32 << 20
	cliMaxStderr       = 64 << 10
)

// CLIClient runs the user's authenticated Inline CLI. It never opens the CLI's
// credential files, supplies account tokens, invokes a shell, or writes media.
type CLIClient struct {
	executable           string
	timeout              time.Duration
	maxStdout, maxStderr int
	accountUserID        int64
}

// NewCLIClient selects an explicit executable, or "inline" from PATH. The first
// Me call checks the installed version, effective production endpoints, and
// authenticated user before an importer creates any archive source.
func NewCLIClient(executable string) *CLIClient {
	if executable == "" {
		executable = "inline"
	}
	return &CLIClient{executable: executable, timeout: 45 * time.Second, maxStdout: cliMaxStdout, maxStderr: cliMaxStderr}
}

func (c *CLIClient) Close() error { return nil }

type cliBoundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	overflow bool
}

func (b *cliBoundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.overflow = true
		b.cancel()
		return 0, errors.New("inline CLI output limit exceeded")
	}
	n, err := b.buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffer Inline CLI output: %w", err)
	}
	return n, nil
}

func (c *CLIClient) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.executable, args...) //nolint:gosec // The operator configures the daemon-host executable; fixed read commands use arguments directly without a shell.
	cmd.WaitDelay = time.Second
	stdout := &cliBoundedBuffer{limit: c.maxStdout, cancel: cancel}
	stderr := &cliBoundedBuffer{limit: c.maxStderr, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("inline CLI output exceeds the bounded response limit")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("inline CLI request stopped: %w", ctx.Err())
	}
	if err != nil {
		// CLI diagnostics can contain account or message data. Preserve the exit
		// condition, but never echo subprocess output into archive logs.
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("inline CLI exited with status %d; check authentication with inline auth me", exitErr.ExitCode())
		}
		return nil, fmt.Errorf("start Inline CLI: %w", err)
	}
	return bytes.Clone(stdout.buffer.Bytes()), nil
}

var cliVersionPattern = regexp.MustCompile(`^inline ([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+][A-Za-z0-9.-]+)?$`)

func (c *CLIClient) Me(ctx context.Context) (Account, error) {
	c.accountUserID = 0
	version, err := c.run(ctx, "--version")
	if err != nil {
		return Account{}, err
	}
	if !cliVersionPattern.Match(bytes.TrimSpace(version)) {
		return Account{}, errors.New("unsupported Inline CLI version response")
	}
	diagnostics, err := c.run(ctx, "doctor", "--json", "--compact")
	if err != nil {
		return Account{}, err
	}
	var doctor struct {
		Config struct {
			APIBaseURL  string `json:"apiBaseUrl"`
			RealtimeURL string `json:"realtimeUrl"`
		} `json:"config"`
	}
	if jsonv2.Unmarshal(diagnostics, &doctor) != nil || !cliProductionEndpoint(doctor.Config.APIBaseURL, "https") || !cliProductionEndpoint(doctor.Config.RealtimeURL, "wss") {
		return Account{}, errors.New("inline CLI must use the production API and realtime endpoints")
	}
	raw, err := c.run(ctx, "auth", "me", "--json", "--compact")
	if err != nil {
		return Account{}, err
	}
	var user struct {
		ID        *int64  `json:"id"`
		FirstName string  `json:"first_name"`
		LastName  *string `json:"last_name"`
		Username  *string `json:"username"`
	}
	if jsonv2.Unmarshal(raw, &user) != nil || user.ID == nil || !cliValidID(*user.ID) {
		return Account{}, errors.New("unsupported Inline CLI authenticated user response: positive integer id required")
	}
	name := strings.TrimSpace(user.FirstName)
	if user.LastName != nil {
		name = strings.TrimSpace(name + " " + *user.LastName)
	}
	if name == "" && user.Username != nil {
		name = *user.Username
	}
	c.accountUserID = *user.ID
	return Account{UserID: *user.ID, Origin: ProductionOrigin, DisplayName: name}, nil
}

func cliProductionEndpoint(value, scheme string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Hostname(), ProductionOrigin) && (u.Port() == "" || u.Port() == "443") && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func cliValidID(id int64) bool { return id > 0 && id <= cliMaxID }

// Discover requests all accessible chats, including unopened linked subthreads.
// The connected backend must support include_subthreads; deploy it before
// enabling complete catalog discovery in the CLI.
func (c *CLIClient) Discover(ctx context.Context) ([]Conversation, error) {
	raw, err := c.run(ctx, "chats", "list", "--include-subthreads", "--json", "--compact")
	if err != nil {
		return nil, fmt.Errorf("complete chat discovery requires an Inline CLI and server supporting --include-subthreads; upgrade Inline if unsupported: %w", err)
	}
	return cliDecodeCatalog(raw, c.accountUserID)
}

func cliDecodeCatalog(raw []byte, accountUserID int64) ([]Conversation, error) {
	var wire struct {
		Chats *[]jsontext.Value `json:"chats"`
	}
	if jsonv2.Unmarshal(raw, &wire) != nil || wire.Chats == nil {
		return nil, errors.New("unsupported Inline CLI chat catalog response")
	}
	conversations := make([]Conversation, 0, len(*wire.Chats))
	seen := make(map[int64]bool, len(*wire.Chats))
	for _, rawChat := range *wire.Chats {
		var chat struct {
			ID *int64 `json:"id"`
		}
		if jsonv2.Unmarshal(rawChat, &chat) != nil || chat.ID == nil || !cliValidID(*chat.ID) {
			return nil, errors.New("unsupported Inline CLI catalog chat ID")
		}
		if seen[*chat.ID] {
			return nil, errors.New("duplicate Inline CLI catalog chat ID")
		}
		conversation, err := cliDecodeConversation(rawChat, *chat.ID, accountUserID)
		if err != nil {
			return nil, err
		}
		seen[*chat.ID] = true
		conversations = append(conversations, conversation)
	}
	sort.Slice(conversations, func(i, j int) bool { return conversations[i].ID < conversations[j].ID })
	return conversations, nil
}

func (c *CLIClient) Conversation(ctx context.Context, chatID int64) (Conversation, error) {
	if !cliValidID(chatID) {
		return Conversation{}, errors.New("invalid Inline chat ID")
	}
	raw, err := c.run(ctx, "chats", "get", "--chat-id", strconv.FormatInt(chatID, 10), "--json", "--compact")
	if err != nil {
		return Conversation{}, err
	}
	var result struct {
		Chat jsontext.Value `json:"chat"`
	}
	if jsonv2.Unmarshal(raw, &result) != nil {
		return Conversation{}, errors.New("unsupported Inline CLI chat response")
	}
	return cliDecodeConversation(result.Chat, chatID, c.accountUserID)
}

func cliDecodeConversation(raw jsontext.Value, chatID, accountUserID int64) (Conversation, error) {
	var chat struct {
		ID              *int64 `json:"id"`
		Title           string `json:"title"`
		SpaceID         *int64 `json:"space_id"`
		ParentChatID    *int64 `json:"parent_chat_id"`
		ParentMessageID *int64 `json:"parent_message_id"`
		Peer            struct {
			Type map[string]jsontext.Value `json:"type"`
		} `json:"peer_id"`
	}
	if jsonv2.Unmarshal(raw, &chat) != nil || chat.ID == nil || *chat.ID != chatID || !cliValidID(chatID) || len(chat.Peer.Type) != 1 {
		return Conversation{}, errors.New("unsupported Inline CLI chat response: canonical chat and native peer required")
	}
	conv := Conversation{ID: chatID, Title: chat.Title, Raw: bytes.Clone(raw)}
	if chat.SpaceID != nil && !cliValidID(*chat.SpaceID) {
		return Conversation{}, errors.New("invalid Inline space ID")
	}
	if chat.ParentChatID != nil {
		if !cliValidID(*chat.ParentChatID) {
			return Conversation{}, errors.New("invalid Inline parent chat ID")
		}
		conv.ParentChatID = *chat.ParentChatID
	}
	if chat.ParentMessageID != nil {
		if !cliValidID(*chat.ParentMessageID) || conv.ParentChatID == 0 {
			return Conversation{}, errors.New("invalid Inline parent message ID")
		}
		conv.RootMessageID = *chat.ParentMessageID
	}
	if peer, ok := chat.Peer.Type["User"]; ok {
		var user struct {
			ID *int64 `json:"user_id"`
		}
		if jsonv2.Unmarshal(peer, &user) != nil || user.ID == nil || !cliValidID(*user.ID) {
			return Conversation{}, errors.New("unsupported Inline CLI direct chat peer")
		}
		conv.Type = "direct_chat"
		if cliValidID(accountUserID) {
			conv.MemberCount, conv.MemberCountKnown = 2, true
			if *user.ID == accountUserID {
				conv.MemberCount = 1
			}
		}
	} else if peer, ok := chat.Peer.Type["Chat"]; ok {
		var thread struct {
			ID *int64 `json:"chat_id"`
		}
		if jsonv2.Unmarshal(peer, &thread) != nil || thread.ID == nil || *thread.ID != chatID {
			return Conversation{}, errors.New("unsupported Inline CLI thread peer")
		}
		conv.Type = "group_chat"
		if chat.SpaceID != nil {
			conv.Type = "channel"
		}
	} else {
		return Conversation{}, errors.New("unsupported Inline CLI peer variant")
	}
	return conv, nil
}

func (c *CLIClient) Messages(ctx context.Context, chatID, beforeID int64) (Page, error) {
	if !cliValidID(chatID) || (beforeID != 0 && !cliValidID(beforeID)) {
		return Page{}, errors.New("invalid Inline history cursor")
	}
	args := []string{"messages", "list", "--chat-id", strconv.FormatInt(chatID, 10), "--limit", strconv.Itoa(cliPageSize)}
	if beforeID != 0 {
		args = append(args, "--offset-id", strconv.FormatInt(beforeID, 10))
	}
	args = append(args, "--json", "--compact")
	raw, err := c.run(ctx, args...)
	if err != nil {
		return Page{}, err
	}
	return cliDecodePage(raw, chatID, beforeID)
}

func cliDecodePage(raw []byte, chatID, beforeID int64) (Page, error) {
	var wire struct {
		Messages *[]jsontext.Value `json:"messages"`
		Page     *struct {
			FetchedCount  *int   `json:"fetchedCount"`
			ReturnedCount *int   `json:"returnedCount"`
			NextOffsetID  *int64 `json:"nextOffsetId"`
			Filtered      *bool  `json:"filtersAppliedToPage"`
		} `json:"page"`
	}
	if jsonv2.Unmarshal(raw, &wire) != nil || wire.Messages == nil || len(*wire.Messages) > cliPageSize {
		return Page{}, errors.New("unsupported Inline CLI history response")
	}
	page := Page{Messages: make([]Message, 0, len(*wire.Messages))}
	seen := make(map[int64]bool)
	for _, rawMessage := range *wire.Messages {
		message, err := cliDecodeMessage(rawMessage, chatID)
		if err != nil {
			return Page{}, err
		}
		if seen[message.ID] || (beforeID != 0 && message.ID >= beforeID) {
			return Page{}, errors.New("inline CLI history did not advance exclusively by message ID")
		}
		seen[message.ID] = true
		page.Messages = append(page.Messages, message)
	}
	sort.Slice(page.Messages, func(i, j int) bool { return page.Messages[i].ID > page.Messages[j].ID })
	var oldest int64
	if len(page.Messages) != 0 {
		oldest = page.Messages[len(page.Messages)-1].ID
	}
	if wire.Page != nil {
		meta := wire.Page
		if meta.Filtered == nil || *meta.Filtered || meta.FetchedCount == nil || meta.ReturnedCount == nil || *meta.FetchedCount != len(page.Messages) || *meta.ReturnedCount != len(page.Messages) || (oldest != 0 && (meta.NextOffsetID == nil || *meta.NextOffsetID != oldest)) || (oldest == 0 && meta.NextOffsetID != nil) {
			return Page{}, errors.New("unsupported or filtered Inline CLI page metadata")
		}
	}
	// Older CLIs have no page metadata. Because this command never supplies
	// filters, the minimum positive ID is a safe exclusive continuation.
	page.HasMore = len(page.Messages) == cliPageSize
	if page.HasMore {
		page.NextBeforeID = oldest
	}
	return page, nil
}

func (c *CLIClient) Files(ctx context.Context, chatID int64, messageIDs []int64) ([]Media, error) {
	if !cliValidID(chatID) || len(messageIDs) > cliPageSize {
		return nil, errors.New("invalid Inline file lookup")
	}
	if len(messageIDs) == 0 {
		return nil, nil
	}
	ids := make([]string, len(messageIDs))
	requested := make(map[int64]bool, len(messageIDs))
	for i, id := range messageIDs {
		if !cliValidID(id) || requested[id] {
			return nil, errors.New("invalid or duplicate Inline file message ID")
		}
		requested[id] = true
		ids[i] = strconv.FormatInt(id, 10)
	}
	raw, err := c.run(ctx, "messages", "get", "--chat-id", strconv.FormatInt(chatID, 10), "--message-id", strings.Join(ids, ","), "--json", "--compact")
	if err != nil {
		return nil, err
	}
	var messages []jsontext.Value
	if len(ids) == 1 {
		messages = []jsontext.Value{raw}
	} else {
		var wire struct {
			Messages *[]jsontext.Value `json:"messages"`
			Missing  []int64           `json:"missingMessageIds"`
		}
		if jsonv2.Unmarshal(raw, &wire) != nil || wire.Messages == nil || len(wire.Missing) != 0 {
			return nil, errors.New("inline CLI file lookup did not return all requested messages")
		}
		messages = *wire.Messages
	}
	if len(messages) != len(requested) {
		return nil, errors.New("inline CLI file lookup returned an incomplete message set")
	}
	var media []Media
	for _, rawMessage := range messages {
		message, err := cliDecodeMessage(rawMessage, chatID)
		if err != nil {
			return nil, err
		}
		if !requested[message.ID] {
			return nil, errors.New("inline CLI file lookup returned an unexpected message")
		}
		delete(requested, message.ID)
		media = append(media, message.Media...)
	}
	return media, nil
}

func cliDecodeMessage(raw jsontext.Value, chatID int64) (Message, error) {
	var wire struct {
		ID       *int64  `json:"id"`
		ChatID   *int64  `json:"chat_id"`
		SenderID *int64  `json:"from_id"`
		Text     *string `json:"message"`
		Date     *int64  `json:"date"`
		EditDate *int64  `json:"edit_date"`
		Out      bool    `json:"out"`
		ReplyID  *int64  `json:"reply_to_msg_id"`
		Media    struct {
			Media map[string]jsontext.Value `json:"media"`
		} `json:"media"`
		Reactions struct {
			Reactions []struct {
				UserID int64  `json:"user_id"`
				Emoji  string `json:"emoji"`
				Date   *int64 `json:"date"`
			} `json:"reactions"`
		} `json:"reactions"`
	}
	if jsonv2.Unmarshal(raw, &wire) != nil || wire.ID == nil || !cliValidID(*wire.ID) || wire.ChatID == nil || *wire.ChatID != chatID || wire.SenderID == nil || !cliValidID(*wire.SenderID) || wire.Date == nil {
		return Message{}, errors.New("unsupported Inline CLI message response: integer message, chat, sender and date required")
	}
	message := Message{ID: *wire.ID, ChatID: chatID, SenderID: *wire.SenderID, SentAt: time.Unix(*wire.Date, 0).UTC(), IsFromMe: wire.Out, Reactions: []Reaction{}, Raw: bytes.Clone(raw), RawFormat: RawCLIFormat}
	if wire.Text != nil {
		message.Text = *wire.Text
	}
	if wire.EditDate != nil {
		message.EditedAt = time.Unix(*wire.EditDate, 0).UTC()
	}
	if wire.ReplyID != nil {
		if !cliValidID(*wire.ReplyID) {
			return Message{}, errors.New("invalid Inline reply message ID")
		}
		message.ReplyToMessageID = *wire.ReplyID
	}
	for _, reaction := range wire.Reactions.Reactions {
		if !cliValidID(reaction.UserID) || reaction.Emoji == "" {
			return Message{}, errors.New("unsupported Inline CLI reaction")
		}
		var createdAt time.Time
		if reaction.Date != nil {
			if *reaction.Date < 0 || *reaction.Date > 253402300799 {
				return Message{}, errors.New("invalid Inline CLI reaction date")
			}
			createdAt = time.Unix(*reaction.Date, 0).UTC()
		}
		message.Reactions = append(message.Reactions, Reaction{UserID: reaction.UserID, Emoji: reaction.Emoji, Count: 1, CreatedAt: createdAt})
	}
	var err error
	message.Media, err = cliDecodeMedia(wire.Media.Media, chatID, message.ID)
	if err != nil {
		return Message{}, err
	}
	return message, nil
}

func cliDecodeMedia(variants map[string]jsontext.Value, chatID, messageID int64) ([]Media, error) {
	if len(variants) == 0 {
		return nil, nil
	}
	if len(variants) != 1 {
		return nil, errors.New("unsupported Inline CLI media variant")
	}
	for kind, raw := range variants {
		if kind == "Nudge" {
			return nil, nil
		}
		var wrapper map[string]jsontext.Value
		if jsonv2.Unmarshal(raw, &wrapper) != nil {
			return nil, errors.New("unsupported Inline CLI media wrapper")
		}
		key := strings.ToLower(kind)
		if key != "photo" && key != "video" && key != "document" && key != "voice" {
			return nil, errors.New("unsupported Inline CLI media type")
		}
		var file struct {
			ID       *int64 `json:"id"`
			Filename string `json:"file_name"`
			MIMEType string `json:"mime_type"`
			URL      string `json:"cdn_url"`
			Size     int64  `json:"size"`
			Width    int    `json:"w"`
			Height   int    `json:"h"`
			Duration int    `json:"duration"`
			Format   int    `json:"format"`
			Sizes    []struct {
				URL    string `json:"cdn_url"`
				Size   int64  `json:"size"`
				Width  int    `json:"w"`
				Height int    `json:"h"`
			} `json:"sizes"`
		}
		if jsonv2.Unmarshal(wrapper[key], &file) != nil || file.ID == nil || !cliValidID(*file.ID) || file.Size < 0 || file.Width < 0 || file.Height < 0 || file.Width > 1<<31-1 || file.Height > 1<<31-1 || file.Duration < 0 || file.Duration > int(^uint(0)>>1)/1000 {
			return nil, errors.New("unsupported Inline CLI media metadata")
		}
		media := Media{ID: key + ":" + strconv.FormatInt(*file.ID, 10), ChatID: chatID, MessageID: messageID, Filename: file.Filename, MIMEType: file.MIMEType, URL: file.URL, Size: file.Size, Width: file.Width, Height: file.Height, DurationMS: file.Duration * 1000, Role: key}
		switch key {
		case "photo":
			media.MIMEType = "image/jpeg"
			if file.Format == 2 {
				media.MIMEType = "image/png"
			}
			for _, size := range file.Sizes {
				if size.Size < 0 || size.Width < 0 || size.Height < 0 || size.Width > 1<<31-1 || size.Height > 1<<31-1 {
					return nil, errors.New("invalid Inline CLI photo size")
				}
				if size.URL != "" && (media.URL == "" || int64(size.Width)*int64(size.Height) > int64(media.Width)*int64(media.Height)) {
					media.URL, media.Size, media.Width, media.Height = size.URL, size.Size, size.Width, size.Height
				}
			}
		case "video":
			media.MIMEType = "video/mp4"
		}
		return []Media{media}, nil
	}
	return nil, nil
}
