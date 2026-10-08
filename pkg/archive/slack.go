package archive

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/slack"
)

// SlackCredential is supplied by the host for each operation. The archive
// never saves it or discovers a token file. BaseURL is empty for Slack's API.
type SlackCredential struct {
	Token   string
	BaseURL string
}

// SlackIdentity is the workspace/principal and grant evidence from auth.test.
type SlackIdentity struct {
	WorkspaceID   string
	WorkspaceName string
	UserID        string
	Scopes        []string
}

// Channel describes a provider conversation and its access metadata.
type Channel struct {
	ID                   string
	Name                 string
	IsPrivate            bool
	IsIM                 bool
	IsGroupIM            bool
	PermissionOverwrites []PermissionOverwrite
}

func slackClient(ctx context.Context, credential SlackCredential) (*slack.Client, SlackIdentity, error) {
	if credential.Token == "" {
		return nil, SlackIdentity{}, errors.New("slack token is required")
	}
	c := slack.NewClient(credential.BaseURL, credential.Token)
	identity, err := c.AuthTest(ctx)
	if err != nil {
		return nil, SlackIdentity{}, err
	}
	scopes := c.Scopes()
	if identity.TeamID == "" || identity.UserID == "" {
		return nil, SlackIdentity{}, errors.New("slack did not identify the workspace and credential user")
	}
	return c, SlackIdentity{WorkspaceID: identity.TeamID, WorkspaceName: identity.Team, UserID: identity.UserID, Scopes: scopes}, nil
}

// InspectSlack returns authenticated identity and scopes without reading content.
// Callers decide which grants are appropriate for their application.
func InspectSlack(ctx context.Context, credential SlackCredential) (SlackIdentity, error) {
	_, identity, err := slackClient(ctx, credential)
	return identity, err
}

// SlackChannels lists accessible conversations for the expected workspace. Empty or
// mismatched workspace evidence is an error; channel names are display-only.
func SlackChannels(ctx context.Context, credential SlackCredential, workspaceID string) ([]Channel, error) {
	c, identity, err := slackClient(ctx, credential)
	if err != nil {
		return nil, err
	}
	if identity.WorkspaceID != workspaceID {
		return nil, errors.New("slack credential belongs to another workspace")
	}
	channels := []Channel{}
	err = c.AllConversations(ctx, func(channel slack.Conversation) error {
		channels = append(channels, Channel{ID: channel.ID, Name: channel.Name,
			IsPrivate: channel.IsPrivate, IsIM: channel.IsIM, IsGroupIM: channel.IsMpim})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return channels, nil
}

// BindSlack validates a credential and returns its archive source. Pass an
// existing source ID to replace the principal without changing retained source
// or message identities. Stop and drain the old synchronization first.
// The first subsequent sync rebuilds
// principal-dependent checkpoints through an idempotent scoped history walk.
func (a *Archive) BindSlack(ctx context.Context, credential SlackCredential, workspaceID string, sourceID int64) (int64, error) {
	_, identity, err := slackClient(ctx, credential)
	if err != nil {
		return 0, err
	}
	if identity.WorkspaceID != workspaceID {
		return 0, errors.New("slack credential belongs to another workspace")
	}
	if sourceID != 0 {
		source, err := a.store.GetSourceByIDContext(ctx, sourceID)
		if err != nil {
			return 0, err
		}
		if source.SourceType != "slack" || !strings.HasPrefix(source.Identifier, workspaceID+":") {
			return 0, errors.New("slack archive source belongs to another workspace")
		}
		return sourceID, nil
	}
	source, err := a.store.GetOrCreateSourceContext(ctx, "slack", identity.WorkspaceID+":"+identity.UserID)
	if err != nil {
		return 0, err
	}
	return source.ID, nil
}

// SlackOptions configures the existing importer, including conversation filters,
// repair, reply discovery, attachment storage, and progress callbacks.
type SlackOptions = slack.ImportOptions

// SlackSync supplies a credential and the options for one synchronization run.
type SlackSync struct {
	Credential SlackCredential
	Options    SlackOptions
}

// SlackSummary reports committed work, including partial progress on failure.
type SlackSummary = slack.ImportSummary

// SyncSlack performs one cancellable run. Zero SourceID discovers the source;
// a supplied source ID keeps its identity across credential replacement.
// Messages count as "from me" when the source's original user sent them, so
// a replacement credential's user does not reattribute retained history.
// Selected ChannelIDs that the credential user cannot list are returned in
// the summary's UnavailableChannels.
func (a *Archive) SyncSlack(ctx context.Context, opts SlackSync) (*SlackSummary, error) {
	c, identity, err := slackClient(ctx, opts.Credential)
	if err != nil {
		return nil, err
	}
	options := opts.Options
	if options.TeamID != "" && identity.WorkspaceID != options.TeamID {
		return nil, errors.New("slack credential belongs to another workspace")
	}
	options.TeamID, options.UserID = identity.WorkspaceID, identity.UserID
	options.RevalidatePrincipal = true
	return slack.NewImporter(a.store, c, identity.WorkspaceID).Import(ctx, options)
}
