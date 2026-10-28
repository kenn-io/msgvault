package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type beeperSendOutput struct {
	Status           string `json:"status"`
	SourceID         int64  `json:"source_id"`
	ChatID           string `json:"chat_id"`
	PendingMessageID string `json:"pending_message_id,omitempty"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	FileName         string `json:"file_name,omitempty"`
}

func (a *storeAPIAdapter) runCLISendMessage(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	intent, err := parseDraftComposeArgs(req.Args)
	if err != nil {
		return err
	}
	if req.Cwd != "" && len(intent.File) == 0 {
		return draftReplyError("invalid_args", errors.New("working directory overrides require a local file"))
	}
	source, err := sourceops.ResolveExactOne(a.store, sourceops.Selector{Account: intent.Account, SourceID: intent.SourceID, SourceIDSet: intent.SourceIDSet})
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	if req.Grant != nil && len(intent.File) != 0 {
		return draftReplyNotPermitted(errors.New("files are owner only"))
	}
	if err := authorizeDelegatedDraftSource(req.Grant, source, agentgrant.PermissionMessageSend); err != nil {
		return err
	}
	if err := authorizeDraftPolicy("beeper", "sends", "send_disabled", a.beeperSendPolicy, source.ID, source.SourceType); err != nil {
		return err
	}
	output := beeperSendOutput{SourceID: source.ID, ChatID: intent.To[0]}
	var file *os.File
	if len(intent.File) != 0 {
		path := intent.File[0]
		if !filepath.IsAbs(path) {
			if req.Cwd == "" {
				return draftReplyError("invalid_args", errors.New("file must be an absolute daemon-host path without a working directory"))
			}
			path = filepath.Join(req.Cwd, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return draftReplyError("invalid_args", err)
		}
		if !info.Mode().IsRegular() {
			return draftReplyError("invalid_args", errors.New("attachment must be a regular file"))
		}
		file, err = os.Open(path)
		if err != nil {
			return draftReplyError("invalid_args", err)
		}
		defer func() { _ = file.Close() }()
		output.FileName = filepath.Base(path)
	}
	chats, err := a.store.ConversationMetadataBatchContext(ctx, source.ID, intent.To)
	if err != nil {
		return draftReplyError("chat_not_archived", err)
	}
	if _, exists := chats[output.ChatID]; !exists {
		return draftReplyError("chat_not_archived", errors.New("chat is not archived for this source"))
	}
	client, err := a.beeperDraftClient()
	if err != nil {
		return err
	}
	chat, err := checkBeeperChat(ctx, client, source, output.ChatID)
	if err != nil {
		return err
	}
	if chat.Merge != nil {
		return draftReplyError("not_supported", errors.New("send to a member chat"))
	}
	if intent.ReplyTo != 0 {
		output.ReplyToMessageID, err = a.checkBeeperReply(ctx, client, source, output.ChatID, intent.ReplyTo)
		if err != nil {
			return err
		}
	}
	params := beeper.SendParams{Text: intent.Body, ReplyToMessageID: output.ReplyToMessageID}
	if file != nil {
		params.UploadID, err = client.UploadAsset(ctx, file, output.FileName)
		if err != nil {
			return draftReplyError(beeperSendFailureCode(err, false), err)
		}
	}
	receipt, err := client.SendMessage(ctx, output.ChatID, params)
	stream := cliStreamStdout
	if err == nil {
		output.Status = "accepted"
		output.ChatID, output.PendingMessageID = receipt.ChatID, receipt.PendingMessageID
	} else {
		output.Status = beeperSendFailureCode(err, true)
		stream = cliStreamStderr
	}
	if emitErr := emitBeeperSend(emit, stream, intent.JSON, output); emitErr != nil {
		return emitErr
	}
	if err != nil {
		return draftReplyError(output.Status, err)
	}
	return nil
}

// checkBeeperReply loads the archived reply target and verifies it against
// the live chat. Store lookup failures are transient; a missing, foreign,
// or live-mismatched target is caller-fixable.
func (a *storeAPIAdapter) checkBeeperReply(ctx context.Context, client *beeper.Client, source *store.Source, chatID string, replyTo int64) (string, error) {
	message, err := a.store.GetMessageContext(ctx, replyTo)
	if err != nil && !errors.Is(err, store.ErrMessageNotFound) {
		return "", draftReplyError("provider_unavailable", err)
	}
	if err != nil || message.SourceID != source.ID || message.SourceConversationID != chatID {
		return "", draftReplyError("invalid_reply_target", errors.New("reply must name an archived message in this chat"))
	}
	if err := client.VerifyArchivedMessage(ctx, chatID, message.SourceMessageID, message.SentAt); err != nil {
		code := "provider_unavailable"
		if errors.Is(err, beeper.ErrNotFound) || errors.Is(err, beeper.ErrArchivedMessageMismatch) {
			code = "invalid_reply_target"
		}
		return "", draftReplyError(code, err)
	}
	return message.SourceMessageID, nil
}

func beeperSendFailureCode(err error, sending bool) string {
	if writeErr, ok := errors.AsType[*beeper.WriteError](err); ok {
		if writeErr.Status >= 300 && writeErr.Status < 500 {
			return "provider_rejected"
		}
		if sending {
			return "remote_unknown"
		}
	}
	return "provider_unavailable"
}

func emitBeeperSend(emit func(api.CLIRunEvent) error, stream string, asJSON bool, output beeperSendOutput) error {
	if emit == nil {
		return nil
	}
	var data string
	if asJSON {
		encoded, err := json.Marshal(output)
		if err != nil {
			return err
		}
		data = string(encoded) + "\n"
	} else {
		data = fmt.Sprintf("status: %s\nbeeper chat: %s\n", output.Status, textutil.SanitizeTerminal(output.ChatID))
		if output.PendingMessageID != "" {
			data += "pending message: " + textutil.SanitizeTerminal(output.PendingMessageID) + "\n"
		}
		if output.FileName != "" {
			data += "file: " + textutil.SanitizeTerminal(output.FileName) + "\n"
		}
		if output.Status == "remote_unknown" {
			data += "Beeper may have sent this message. Read the chat before sending again.\n"
		}
	}
	return emit(api.CLIRunEvent{Type: stream, Data: data})
}
