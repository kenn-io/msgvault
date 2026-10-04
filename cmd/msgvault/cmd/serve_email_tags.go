package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.MessageTagStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error) {
	ctx = a.invocationContext(ctx)
	if change != nil {
		var err error
		normalized, err := emailtags.Normalize(*change, false)
		if err != nil {
			return nil, err
		}
		change = &normalized
		mailbox = change.Mailbox
	}
	target, err := a.store.EmailTagTargetContext(ctx, id, mailbox)
	if err != nil {
		return nil, err
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.SourceID)
	if err != nil {
		code := "local_failed"
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			code = "sync_active"
		}
		return nil, emailtags.Failure(code, "Cannot acquire source ownership for tag editing", nil, err)
	}
	defer func() { _ = execution.Release() }()
	// Resolve again under source ownership; never substitute a newly mapped source.
	locked, err := a.store.EmailTagTargetContext(ctx, id, mailbox)
	if err != nil {
		return nil, err
	}
	if locked != target {
		return nil, emailtags.Failure("stale_identity", "Message identity changed; sync and retry", nil, nil)
	}
	source, err := a.store.GetSourceByIDContext(ctx, target.SourceID)
	if err != nil {
		return nil, emailtags.Failure("local_failed", "Cannot resolve tag source", nil, err)
	}
	result, err := a.providerMessageTags(ctx, source, target, change)
	if result != nil {
		result.MessageID, result.SourceID = id, target.SourceID
	}
	if err != nil {
		return result, err
	}
	if change == nil || change.DryRun {
		return result, nil
	}
	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	if err := a.store.SaveEmailTagsContext(evidenceCtx, target, result); err != nil {
		return result, emailtags.Failure("remote_accepted_local_failed", "Provider tags were verified but could not be saved locally; sync the account", result, err)
	}
	if err := execution.Release(); err != nil {
		return result, emailtags.Failure("local_failed", "Provider tags were saved but source ownership could not be released", result, err)
	}
	if target.Provider == "gmail" || target.Provider == sourceTypeMSMail {
		a.refreshDraftCache(evidenceCtx, source)
	}
	return result, nil
}

func (a *storeAPIAdapter) providerMessageTags(ctx context.Context, source *store.Source, target store.EmailTagTarget, change *emailtags.Change) (*emailtags.Result, error) {
	if target.Provider == sourceTypeMSMail {
		write := change != nil && !change.DryRun
		state := invocationFromContext(ctx)
		if write {
			granted, err := newGraphMailWriteManager(state).HasScopes(source.Identifier)
			if err != nil || !granted {
				return nil, emailtags.Failure("insufficient_scope", "Microsoft category editing requires Mail.ReadWrite; reauthorize with add-o365 --graph --mail-write", nil, err)
			}
		}
		factory := a.microsoftTagClientFactory
		if factory == nil {
			factory = func(ctx context.Context, source *store.Source, write bool) (*msmail.Client, error) {
				manager := newGraphMailManager(state)
				if write {
					manager = newGraphMailWriteManager(state)
				}
				token, err := manager.TokenSource(ctx, source.Identifier)
				if err != nil {
					return nil, err
				}
				return msmail.NewClient(msmail.GraphBaseURL, token, msmailQPS), nil
			}
		}
		client, err := factory(ctx, source, write)
		if err != nil {
			return nil, emailtags.Failure("provider_unavailable", "Cannot connect to the Microsoft mailbox", nil, err)
		}
		return client.MessageTags(ctx, target.SourceMessageID, change)
	}
	scopes := []string{oauth.ScopeGmailModify, oauth.ScopeGmailFull}
	if target.Provider == "gmail" && change != nil {
		if err := gmailDraftScopeGate(ctx, a.config, source, scopes); err != nil {
			return nil, emailtags.Failure("insufficient_scope", "Gmail tag editing requires gmail.modify; reauthorize the account with add-account --force", nil, err)
		}
	}
	factory := a.emailTagClientFactory
	if factory == nil {
		factory = func(ctx context.Context, source *store.Source) (gmail.API, error) {
			requestedScopes := oauth.ScopesGmailReadonly
			if change != nil {
				requestedScopes = []string{oauth.ScopeGmailModify}
			}
			state := invocationFromContext(ctx)
			if source.SourceType == sourceTypeIMAP {
				return buildAPIClient(ctx, source, oauthManagerCache(state), requestedScopes)
			}
			client, _, err := newDaemonGmailClient(
				ctx, source.Identifier, source, oauthManagerCache(state), state, requestedScopes,
			)
			return client, err
		}
	}
	client, err := factory(ctx, source)
	if err != nil {
		if credentialErr, ok := errors.AsType[*provideridentity.GmailCredentialError](err); ok {
			if remediation := credentialErr.Remediation(); remediation != "" {
				return nil, emailtags.Failure("provider_unavailable", remediation, nil, err)
			}
		}
		return nil, emailtags.Failure("provider_unavailable", "Cannot connect to the message provider", nil, err)
	}
	defer func() { _ = client.Close() }()
	if target.Provider == "imap" {
		imapClient, ok := client.(*imaplib.Client)
		if !ok {
			return nil, emailtags.Failure("unsupported_provider", "Source does not support IMAP keyword editing", nil, nil)
		}
		return imapClient.MessageKeywords(ctx, imaplib.KeywordIdentity{Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID}, change)
	}
	tagClient, ok := client.(gmail.MessageTagClient)
	if !ok {
		return nil, emailtags.Failure("unsupported_provider", "Source does not support Gmail label editing", nil, nil)
	}
	return tagClient.MessageTags(ctx, target.SourceMessageID, change)
}
