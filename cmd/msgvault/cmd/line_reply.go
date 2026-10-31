package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func init() { rootCmd.AddCommand(newSendLineReplyCommand()) }

func newSendLineReplyCommand() *cobra.Command {
	command := &cobra.Command{Use: "send-line-reply <draft-id>", Short: "Send a reviewed LINE chat draft (owner only)", Args: cobra.ExactArgs(1), RunE: runDaemonCLICommandHTTPFromCobra}
	command.Flags().Int64("revision", 0, "reviewed draft revision")
	_ = command.MarkFlagRequired("revision")
	command.Flags().Bool("json", false, "emit the send receipt as JSON")
	return command
}

func (a *storeAPIAdapter) runCLISendLineReply(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if req.Grant != nil {
		return draftReplyNotPermitted(errors.New("LINE sending requires the owner"))
	}
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("LINE sending does not accept environment or directory overrides"))
	}
	intent, err := parseDraftLifecycleArgs(req.Args)
	if err != nil {
		return err
	}
	if strings.HasPrefix(intent.DraftID, "-") {
		return draftReplyError("invalid_args", errors.New("invalid draft ID"))
	}
	sender := line.ReplySender{Store: a.store, Client: func(_ context.Context, attempt store.LineReplyAttempt) (*line.Client, error) {
		if a.config == nil {
			return nil, draftReplyError("send_disabled", errors.New("LINE configuration unavailable"))
		}
		cfg, err := config.Load(a.config.ConfigFilePath(), a.config.HomeDir)
		if err != nil {
			return nil, draftReplyError("send_disabled", err)
		}
		profile := cfg.GetLineSource(attempt.ChannelID)
		if profile == nil || !profile.Enabled || !profile.AllowSend {
			return nil, draftReplyError("send_disabled", errors.New("LINE channel has not opted into sending"))
		}
		client, err := configuredLineClient(*profile)
		if err != nil {
			return nil, draftReplyError("credentials_unavailable", err)
		}
		return client, nil
	}}
	receipt, sendErr := sender.Send(ctx, intent.DraftID, intent.Revision)
	if receipt.Status == store.LineReplySent {
		// The sender released source ownership before returning. Refresh even if
		// the caller disconnected or this is recovery of an already durable send.
		defer func() {
			evidenceCtx, cancel := localDraftEvidenceContext(ctx)
			defer cancel()
			source, lookupErr := a.store.GetSourceByIDContext(evidenceCtx, receipt.SourceID)
			if lookupErr == nil {
				a.refreshDraftCache(evidenceCtx, source)
			} else if a.logger != nil {
				a.logger.Error("LINE reply cache source lookup failed", "source_id", receipt.SourceID, "error", lookupErr)
			}
		}()
	}
	if sendErr != nil {
		if !receipt.Accepted {
			if errors.Is(sendErr, line.ErrInvalidReplyText) {
				return draftReplyError("invalid_args", sendErr)
			}
			if errors.Is(sendErr, store.ErrSyncAlreadyActive) {
				return draftReplyError("pending_operation", sendErr)
			}
			return chatDraftStoreError(sendErr, nil)
		}
		// Preserve the provider receipt in CLI/MCP output when local adoption fails.
		// The same reviewed command resumes recovery; it never admits a new send.
		if a.logger != nil {
			a.logger.Error("LINE reply recovery pending", "draft_id", receipt.DraftID, "error", sendErr)
		}
	}
	var data string
	if intent.JSON {
		encoded, err := json.Marshal(receipt)
		if err != nil {
			return draftReplyError("output_failed", err)
		}
		data = string(encoded) + "\n"
	} else {
		data = fmt.Sprintf("draft=%s revision=%d status=%s accepted=%t message_id=%d remote_message_id=%s\n", textutil.SanitizeTerminal(receipt.DraftID), receipt.Revision, receipt.Status, receipt.Accepted, receipt.MessageID, textutil.SanitizeTerminal(receipt.RemoteMessageID))
		if receipt.RetryAfter != nil {
			data += fmt.Sprintf("retry_after=%s\n", receipt.RetryAfter.Format("2006-01-02T15:04:05Z07:00"))
		}
	}
	if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: data}); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}
