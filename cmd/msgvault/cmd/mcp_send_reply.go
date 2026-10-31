package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	xapi "go.kenn.io/msgvault/internal/x"
)

type daemonMCPReplySender struct{ client *daemonclient.Client }

func (r daemonMCPReplySender) PrepareReply(ctx context.Context, request mcpserver.ReplyRequest) (xapi.ReplyPreparation, error) {
	if request.MessageID != 0 {
		// Resolve the actual source before generic draft-reply: an unsupported mail
		// parent must not create a remote mail draft as a side effect of sending.
		parent, err := r.client.GetMessageContext(ctx, request.MessageID)
		if err != nil {
			return xapi.ReplyPreparation{}, err
		}
		accounts, _, err := r.client.GetCLIAccounts(ctx)
		if err != nil {
			return xapi.ReplyPreparation{}, err
		}
		kind := ""
		for _, account := range accounts {
			if account.ID == parent.SourceID {
				kind = account.Type
				break
			}
		}
		if kind == "" {
			return xapi.ReplyPreparation{}, errors.New("reply source is unavailable")
		}
		if kind != "x" {
			return xapi.ReplyPreparation{}, fmt.Errorf("replies are not supported for %s sources", kind)
		}
		output, err := (daemonMCPDraftRunner{client: r.client}).RunDraftCommand(ctx, mcpserver.DraftCommandRequest{Command: "draft-reply", Positional: strconv.FormatInt(request.MessageID, 10), Flags: map[string][]string{"body": {request.Body}}})
		if err != nil {
			return xapi.ReplyPreparation{}, err
		}
		var draft chatDraftOutput
		if err := json.Unmarshal([]byte(output.Stdout), &draft); err != nil {
			return xapi.ReplyPreparation{}, fmt.Errorf("decode prepared reply draft: %w", err)
		}
		request.DraftID, request.Revision = draft.DraftID, draft.Revision
	}
	return r.client.PrepareReply(ctx, request.DraftID, request.Revision, request.AdoptPost)
}
func (r daemonMCPReplySender) SendReply(ctx context.Context, request mcpserver.ReplyRequest) (xapi.SendResult, error) {
	var result xapi.SendResult
	if request.MessageID != 0 || request.Body != "" || request.DraftID == "" || request.Revision <= 0 {
		return result, errors.New("send requires the prepared draft and revision")
	}
	args := []string{"send-reply", "--draft", request.DraftID, "--revision", strconv.FormatInt(request.Revision, 10), "--json"}
	if request.AdoptPost != "" {
		args = append(args, "--adopt-post", request.AdoptPost)
	}
	var output strings.Builder
	err := r.client.RunCLICommand(ctx, daemonclient.CLIRunRequest{Args: args}, func(stream, data string) error {
		if stream == cliStreamStdout {
			output.WriteString(data)
		}
		return nil
	})
	if output.Len() > 0 {
		if decodeErr := json.Unmarshal([]byte(output.String()), &result); decodeErr != nil {
			return result, fmt.Errorf("decode reply outcome: %w", decodeErr)
		}
	}
	if err == nil && result.DraftID == "" {
		return result, errors.New("reply sender returned no durable outcome")
	}
	return result, err
}
