package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

func replySurfaceError(err error) error {
	if err == nil {
		return nil
	}
	var coded *api.CLIRunCodedError
	if errors.As(err, &coded) {
		return err
	}
	var reply *xapi.ReplyError
	if errors.As(err, &reply) {
		return xRegisterError(reply.Code, err)
	}
	return chatDraftStoreError(err, nil)
}
func unsupportedReplySource(kind string) error {
	return xRegisterError(fmt.Sprintf("replies are not supported for %s sources", kind), nil)
}

// replySender resolves receipts before drafts, so status/repair survives draft
// deletion, credential revocation and write opt-out. New work gets current policy.
func (a *storeAPIAdapter) replySender(ctx context.Context, req api.ReplyPrepareRequest) (*xapi.Sender, error) {
	if a.store == nil {
		return nil, xRegisterError("unavailable", nil)
	}
	receipt, err := a.store.GetXReplyReceiptContext(ctx, req.DraftID)
	var source *store.Source
	if err == nil {
		source, err = a.store.GetSourceByIDContext(ctx, receipt.SourceID)
	} else if errors.Is(err, store.ErrXReplyReceiptNotFound) {
		draft, loadErr := a.store.GetChatDraftContext(ctx, req.DraftID)
		if loadErr != nil {
			return nil, replySurfaceError(loadErr)
		}
		if draft.SourceType != "x" {
			return nil, unsupportedReplySource(draft.SourceType)
		}
		source, err = a.store.GetSourceByIDContext(ctx, draft.SourceID)
	}
	if err != nil {
		return nil, replySurfaceError(err)
	}
	if source.SourceType != "x" {
		return nil, unsupportedReplySource(source.SourceType)
	}
	// Terminal outcomes must not be gated on current configuration or credentials.
	if receipt.State == "sent" || receipt.State == "remote_accepted" || (receipt.State != "" && receipt.State != "rejected" && req.AdoptPost == "") {
		return xapi.NewSender(a.store, nil, config.XAccountConfig{}, nil), nil
	}
	if a.config == nil {
		return nil, xRegisterError("unavailable", nil)
	}
	policy, err := a.config.XAccount(source.Identifier)
	if err != nil {
		return nil, xRegisterError("invalid_policy", err)
	}
	deps := defaultXCommandDeps()
	if a.xDeps != nil {
		deps = *a.xDeps
	}
	credentials := deps.manager(a.config.TokensDir()).Credentials(source.Identifier, deps.httpClient)
	return xapi.NewSender(a.store, xapi.NewClient(deps.baseURL(), credentials.HTTPClient()), policy, credentials), nil
}
func (a *storeAPIAdapter) PrepareReply(ctx context.Context, req api.ReplyPrepareRequest, grant *agentgrant.Grant) (xapi.ReplyPreparation, error) {
	if grant != nil {
		return xapi.ReplyPreparation{}, xRegisterError("not_permitted", nil)
	}
	if err := req.Validate(); err != nil {
		return xapi.ReplyPreparation{}, xRegisterError("invalid_args", err)
	}
	sender, err := a.replySender(ctx, req)
	if err != nil {
		return xapi.ReplyPreparation{}, err
	}
	result, err := sender.PrepareReply(ctx, xapi.SendRequest{DraftID: req.DraftID, Revision: req.Revision, AdoptPost: req.AdoptPost, Owner: true})
	return result, errors.Join(replySurfaceError(err), a.finishReplyCleanup(ctx))
}

// Sender has released source execution before the daemon takes ownership of
// cleanup spanning other accounts. Preserve lookup evidence on error exits and
// caller cancellation, using the same coordinator that fences daemon readers.
func (a *storeAPIAdapter) finishReplyCleanup(ctx context.Context) error {
	if a.xCleanup == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := a.xCleanup.Resume(cleanupCtx); err != nil {
		return xRegisterError("x_content_cleanup_pending", err)
	}
	return nil
}

func (a *storeAPIAdapter) runCLISendReply(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) (retErr error) {
	if req.Grant != nil || req.GrantDecided {
		return xRegisterError("not_permitted", nil)
	}
	intent, err := api.ParseCLIRunSendReply(req)
	if err != nil {
		return xRegisterError("invalid_args", err)
	}
	if intent.MessageID != 0 {
		source, err := a.store.GetMessageSourceContext(ctx, intent.MessageID)
		if err != nil {
			return replySurfaceError(err)
		}
		if source.SourceType != "x" {
			return unsupportedReplySource(source.SourceType)
		}
		// Reuse the production draft-reply algorithm. Its durable output must reach
		// the transport before Send can admit/post; write failure stops here.
		err = a.runCLIReplyDraft(ctx, api.CLIRunRequest{Args: []string{"draft-reply", fmt.Sprint(intent.MessageID), "--body", intent.Body, "--json"}}, func(e api.CLIRunEvent) error {
			if e.Type != cliStreamStdout {
				return emit(e)
			}
			var draft chatDraftOutput
			if err := json.Unmarshal([]byte(e.Data), &draft); err != nil {
				return err
			}
			intent.DraftID, intent.Revision = draft.DraftID, draft.Revision
			data := fmt.Sprintf("Prepared draft %s revision %d; retain this handle for recovery.\n", draft.DraftID, draft.Revision)
			if intent.JSON {
				encoded, err := json.Marshal(xapi.SendResult{Status: "prepared", DraftID: draft.DraftID, Revision: draft.Revision})
				if err != nil {
					return err
				}
				data = string(encoded) + "\n"
			}
			return emit(api.CLIRunEvent{Type: cliStreamStderr, Data: data})
		})
		if err != nil {
			return err
		}
	}
	sender, err := a.replySender(ctx, intent.ReplyPrepareRequest)
	if err != nil {
		return err
	}
	// Emit any durable result first, even if subsequent local cleanup fails.
	defer func() { retErr = errors.Join(retErr, a.finishReplyCleanup(ctx)) }()
	result, sendErr := sender.Send(ctx, xapi.SendRequest{DraftID: intent.DraftID, Revision: intent.Revision, AdoptPost: intent.AdoptPost, Owner: true})
	if result.DraftID != "" {
		data := fmt.Sprintf("%s: draft %s revision %d, attempt %s, remote ID %s, reserved USD %d.%06d\n", result.Status, result.DraftID, result.Revision, result.AttemptID, result.RemoteID, result.ReservedMicros/1000000, result.ReservedMicros%1000000)
		if intent.JSON {
			encoded, err := json.Marshal(result)
			if err != nil {
				return xRegisterError("output_failed", err)
			}
			data = string(encoded) + "\n"
		}
		if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: data}); err != nil {
			return xRegisterError("output_failed", err)
		}
		if result.Status == "remote_unknown" {
			if err := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "remote_unknown: X may have accepted this reply. Do not submit another draft to retry; reconcile this attempt or explicitly adopt its remote post.\n"}); err != nil {
				return xRegisterError("output_failed", err)
			}
		}
	}
	// Never pass provider causes or submitted body into public stream errors.
	if sendErr != nil {
		return replySurfaceError(sendErr)
	}
	return nil
}

var _ api.ReplyPreparer = (*storeAPIAdapter)(nil)
