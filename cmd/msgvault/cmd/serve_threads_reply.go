package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"strconv"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/threads"
)

// runCLIThreadsSendReply runs inside the HTTP operation gate. Lifecycle and
// source execution ownership are released before publishing derived caches.
func (a *storeAPIAdapter) runCLIThreadsSendReply(
	ctx context.Context,
	req api.CLIRunRequest,
	intent api.SendReplyIntent,
	source *store.Source,
	emit func(api.CLIRunEvent) error,
) error {
	coded := func(code string, err error) error { return &api.CLIRunCodedError{Code: code, Err: err} }
	if req.Grant != nil {
		return coded("not_permitted", threads.ErrReplyNotPermitted)
	}
	if source.SourceType != sourceTypeThreads || source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return coded("not_permitted", threads.ErrReplyNotPermitted)
	}
	appID, ownerID, err := threads.ParseSourceIdentifier(source.Identifier)
	if err != nil {
		return coded("not_permitted", err)
	}
	deps := defaultThreadsCommandDepsForContext(ctx)
	if a.threadsReplyDeps != nil {
		deps = *a.threadsReplyDeps
	}
	budget := threads.NewRequestBudget(threads.BudgetOptions{MaxAttempts: threads.ManualRequestLimit, Now: deps.now, Wait: deps.wait})
	runCtx, cancel := budget.RunContext(ctx)
	defer cancel()
	manager := deps.tokenManager()
	var result threads.SendResult
	var quota threads.ReplyQuota
	var record threads.TokenRecord
	draftID := intent.DraftID
	revision := intent.Revision
	failureCode := "reply_unavailable"
	runErr := manager.WithSourceLock(runCtx, source.Identifier, func() (lockedErr error) {
		current, err := a.store.GetSourceByIDContext(runCtx, source.ID)
		if err != nil {
			return err
		}
		if current.SourceType != sourceTypeThreads || current.Identifier != source.Identifier || current.HistoryOnly || current.MergedIntoSourceID != 0 {
			return store.ErrSourceRetired
		}
		execution, err := a.store.AcquireSyncExecutionContext(runCtx, source.ID)
		if err != nil {
			return err
		}
		defer func() { lockedErr = errors.Join(lockedErr, execution.Release()) }()
		defer func() {
			if apiErr, ok := errors.AsType[*threads.APIError](lockedErr); ok && apiErr.CredentialInvalid() && record.AppID != "" {
				lockedErr = errors.Join(lockedErr, manager.Invalidate(record))
			}
		}()

		if deps.limiter != nil {
			budget.BindLimiter(deps.limiter(source.Identifier))
		}
		if !intent.Quota {
			if draftID != "" {
				receipt, receiptErr := a.store.GetThreadsReplySendContext(runCtx, draftID)
				if receiptErr == nil {
					if receipt.SourceID != source.ID || receipt.SourceIdentifier != source.Identifier {
						failureCode = "not_permitted"
						return threads.ErrReplyNotPermitted
					}
				} else {
					if !errors.Is(receiptErr, store.ErrThreadsReplyNotFound) {
						failureCode = "draft_unavailable"
						return receiptErr
					}
					draft, draftErr := a.store.GetChatDraftContext(runCtx, draftID)
					if draftErr != nil {
						failureCode = "draft_unavailable"
						return draftErr
					}
					if draft.SourceID != source.ID || draft.SourceIdentifier != source.Identifier || draft.SourceType != sourceTypeThreads {
						failureCode = "not_permitted"
						return threads.ErrReplyNotPermitted
					}
				}
			} else {
				if err := threads.ValidateReplyBody(intent.Body); err != nil {
					failureCode = "invalid_body"
					return err
				}
				draftID = threadsDirectReplyDraftID(source.ID, intent.MessageID, intent.Body)
				receipt, receiptErr := a.store.GetThreadsReplySendContext(runCtx, draftID)
				if receiptErr == nil {
					if receipt.SourceID != source.ID || receipt.SourceIdentifier != source.Identifier ||
						receipt.Body != intent.Body || receipt.DraftRevision != 1 {
						failureCode = "not_permitted"
						return threads.ErrReplyNotPermitted
					}
					revision = receipt.DraftRevision
				} else {
					if !errors.Is(receiptErr, store.ErrThreadsReplyNotFound) {
						failureCode = "draft_unavailable"
						return receiptErr
					}
					if !threadsSourceSendEnabled(a.config, source.ID) {
						failureCode = "reply_not_permitted"
						return threads.ErrReplyNotPermitted
					}
					target, err := a.store.GetMessageContext(runCtx, intent.MessageID)
					if err != nil {
						failureCode = "invalid_target"
						return err
					}
					if target.SourceID != source.ID || target.MessageType != "threads" || target.ConversationID <= 0 || target.SourceConversationID == "" || target.SourceMessageID == "" {
						failureCode = "not_permitted"
						return threads.ErrReplyNotPermitted
					}
					if _, err := a.store.GetThreadsOwnRootContext(runCtx, source.ID, target.SourceConversationID); err != nil {
						failureCode = "not_permitted"
						return threads.ErrReplyNotPermitted
					}
					if err := a.store.ValidateThreadsReplyTargetContext(runCtx, source.ID, target.ConversationID, target.SourceConversationID, target.SourceMessageID); err != nil {
						failureCode = "not_permitted"
						return threads.ErrReplyNotPermitted
					}
					draft, err := a.store.CreateChatDraftWithIDContext(runCtx, draftID, target.ConversationID, target.ID, intent.Body, func(string, string) error { return nil })
					if err != nil {
						failureCode = "invalid_target"
						return err
					}
					revision = draft.Revision
				}
			}
		}
		openAPI := func(callCtx context.Context, id int64) (threads.PublishAPI, []string, error) {
			if id != source.ID {
				return nil, nil, threads.ErrReplyNotPermitted
			}
			required := []string{"threads_basic", "threads_content_publish", "threads_manage_replies"}
			if intent.AdoptMediaID != "" {
				required = []string{"threads_basic", "threads_read_replies"}
			}
			saved, err := manager.Load(appID, ownerID)
			if err != nil {
				return nil, nil, err
			}
			for _, scope := range required {
				if !slices.Contains(saved.Scopes, scope) {
					return nil, nil, threads.ErrReplyNotPermitted
				}
			}
			record, err = manager.EnsureCurrent(callCtx, source.Identifier, deps.oauthOptions(budget))
			if err != nil {
				return nil, nil, err
			}
			for _, scope := range required {
				if !slices.Contains(record.Scopes, scope) {
					return nil, nil, threads.ErrReplyNotPermitted
				}
			}
			client, err := deps.newAPI(record, budget)
			if err != nil {
				return nil, nil, err
			}
			publishing, ok := client.(threads.PublishAPI)
			if !ok {
				return nil, nil, threads.ErrReplyNotPermitted
			}
			return publishing, record.Scopes, nil
		}
		if intent.Quota {
			client, _, err := openAPI(runCtx, source.ID)
			if err != nil {
				return err
			}
			quota, err = client.ReplyQuota(runCtx)
			return err
		}
		sender := threads.NewSender(a.store, threads.SenderOptions{
			Authorize: func(context.Context) error {
				if req.Grant != nil {
					return threads.ErrReplyNotPermitted
				}
				return nil
			},
			LoadPolicy: func(callCtx context.Context, id int64) (threads.SendPolicy, error) {
				fresh, err := a.store.GetSourceByIDContext(callCtx, id)
				if err != nil {
					return threads.SendPolicy{}, err
				}
				enabled := threadsSourceSendEnabled(a.config, id)
				return threads.SendPolicy{Enabled: enabled, SourceIdentifier: fresh.Identifier}, nil
			}, OpenAPI: openAPI, Now: deps.now, Wait: deps.wait,
		})
		result, err = sender.Send(runCtx, threads.SendRequest{DraftID: draftID, Revision: revision, AdoptMediaID: intent.AdoptMediaID})
		return err
	})
	if intent.Quota {
		if runErr != nil {
			return coded("quota_unavailable", runErr)
		}
		return emitSendReplyJSON(emit, quota)
	}
	if result.DraftID == "" {
		return coded(failureCode, runErr)
	}
	if (result.ArchiveComplete || result.MessageID != 0) && a.draftCacheRefresh != nil {
		if cacheErr := a.draftCacheRefresh(context.WithoutCancel(ctx), source.Identifier); cacheErr != nil {
			result.LocalStatus = "cache_pending"
			result.Status = "sent_archive_pending"
			result.ErrorCategory = "cache_failed"
			runErr = errors.Join(runErr, cacheErr)
		}
	}
	if result.LocalStatus != "complete" && runErr == nil {
		runErr = errors.New("Threads reply requires inspection or completion")
	}
	outputErr := emitSendReplyJSON(emit, result)
	if runErr != nil {
		return errors.Join(outputErr, coded(result.Status, runErr))
	}
	return outputErr
}
func emitSendReplyJSON(emit func(api.CLIRunEvent) error, value any) error {
	if emit == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return emit(api.CLIRunEvent{Type: "stdout", Data: string(data) + "\n"})
}

func threadsSourceSendEnabled(cfg *config.Config, sourceID int64) bool {
	if cfg == nil {
		return false
	}
	for _, entry := range cfg.Threads.Sends {
		if entry.SourceID == sourceID {
			return true
		}
	}
	return false
}

func threadsDirectReplyDraftID(sourceID, messageID int64, body string) string {
	identity := strconv.FormatInt(sourceID, 10) + "\x00" + strconv.FormatInt(messageID, 10) + "\x00" + body
	sum := sha256.Sum256([]byte(identity))
	return store.ChatDraftIDPrefix + "direct-" + hex.EncodeToString(sum[:])
}
