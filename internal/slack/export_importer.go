package slack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	attachmentexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const sourceTypeSlackdump = "slackdump"

var errSlackdumpMessageLimitReached = errors.New("slackdump message limit reached")

// SlackdumpImportOptions configures one offline Slackdump import.
type SlackdumpImportOptions struct {
	// Me is the importing Slack user's ID or profile email.
	Me string
	// Limit caps messages processed per conversation (zero means unlimited).
	Limit int
	// AttachmentsDir is populated by the attachment import path.
	AttachmentsDir string
	// MaxMediaBytes caps one exported attachment (zero uses the default).
	MaxMediaBytes int64
	// MediaPolicy is the resolved attachment retention policy. A
	// MediaPolicyForTeam result replaces it after the export identity is known.
	MediaPolicy attachmentpolicy.Policy
	// MediaPolicyForTeam resolves workspace-specific retention settings after
	// the export identity is known.
	MediaPolicyForTeam func(string) attachmentpolicy.Policy
	// Progress receives one status line after each conversation.
	Progress func(string)
}

// SlackdumpImportSummary uses the live Slack counters and adds the number of
// exported file records whose bytes were absent from the snapshot.
type SlackdumpImportSummary struct {
	ImportSummary

	AttachmentsMissing int
}

// ImportSlackdump imports a Slackdump standard export without contacting
// Slack. Directory and ZIP inputs share the same reader and persistence path.
func ImportSlackdump(
	ctx context.Context,
	st *store.Store,
	inputPath string,
	opts SlackdumpImportOptions,
) (summary *SlackdumpImportSummary, err error) {
	started := time.Now()
	if st == nil {
		return nil, errors.New("slackdump import requires a store")
	}
	if opts.Limit < 0 {
		return nil, errors.New("slackdump import limit must be non-negative")
	}

	export, err := openSlackdumpExport(inputPath)
	if err != nil {
		return nil, err
	}
	exportClosed := false
	defer func() {
		if exportClosed {
			return
		}
		if closeErr := export.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close Slackdump export %s: %w", inputPath, closeErr)
		}
	}()

	users, err := export.users()
	if err != nil {
		return nil, err
	}
	me, err := resolveSlackdumpIdentity(users, opts.Me)
	if err != nil {
		return nil, err
	}
	if me.TeamID == "" {
		return nil, fmt.Errorf("slackdump user %s has no team ID", me.ID)
	}
	if opts.MediaPolicyForTeam != nil {
		opts.MediaPolicy = opts.MediaPolicyForTeam(me.TeamID)
	}
	if opts.MaxMediaBytes > 0 {
		opts.MediaPolicy.MaxBytes = opts.MaxMediaBytes
	}
	if err = opts.MediaPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("slackdump media policy: %w", err)
	}
	conversations, err := export.conversations(me.ID)
	if err != nil {
		return nil, err
	}

	source, err := st.GetOrCreateSource(sourceTypeSlackdump, me.TeamID+":"+me.ID)
	if err != nil {
		return nil, err
	}
	summary = &SlackdumpImportSummary{}
	summary.SourceID = source.ID
	defer func() {
		summary.Duration = time.Since(started)
	}()

	syncID, err := st.StartSync(source.ID, sourceTypeSlackdump)
	if err != nil {
		return summary, err
	}
	scopedStore := st.ScopedToSync(source.ID, syncID)
	importer := &Importer{
		store: scopedStore,
		res:   newParticipantResolver(scopedStore, me.TeamID),
		now:   time.Now,
	}
	importer.res.loadUserSnapshot(users)
	importer.opts = ImportOptions{
		TeamID:         me.TeamID,
		UserID:         me.ID,
		Limit:          opts.Limit,
		NoMedia:        true,
		MaxMediaBytes:  opts.MaxMediaBytes,
		AttachmentsDir: opts.AttachmentsDir,
		Progress:       opts.Progress,
	}
	importer.sourceID = source.ID
	defer func() {
		if err != nil {
			_ = scopedStore.FailSyncWithCheckpoint(syncID, err.Error(), slackdumpCheckpoint(summary))
		}
	}()

	for index := range conversations {
		if err = ctx.Err(); err != nil {
			return summary, err
		}
		conversation := &conversations[index]
		var conversationID int64
		conversationID, err = scopedStore.EnsureConversationWithType(
			source.ID,
			conversation.ID,
			conversationType(conversation),
			conversationTitle(conversation, importer.res.displayName),
		)
		if err != nil {
			return summary, err
		}

		var toRecipients []messageRecipient
		var participantCount int
		toRecipients, participantCount, err = ensureSlackdumpMembership(
			scopedStore, importer.res, conversationID, conversation, me.ID, opts.MediaPolicy,
		)
		if err != nil {
			return summary, err
		}

		before := summary.MessagesProcessed
		cc := &convScope{
			channelID:              conversation.ID,
			convID:                 conversationID,
			sourceID:               source.ID,
			syncID:                 syncID,
			opts:                   importer.opts,
			toRecipients:           toRecipients,
			filesHandledExternally: true,
		}
		cc.opts.MediaConversation = attachmentpolicy.Conversation{
			Type:             conversationType(conversation),
			ParticipantCount: participantCount,
		}
		err = export.walkMessages(*conversation, func(message *Message) error {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = importer.processMessage(ctx, cc, message, &summary.ImportSummary); err != nil {
				return fmt.Errorf("import Slackdump conversation %s: %w", conversation.ID, err)
			}
			if message.Type == "message" && message.TS != "" {
				var messageIDs map[string]int64
				messageIDs, err = scopedStore.MessageExistsBatch(source.ID, []string{
					sourceMessageID(conversation.ID, message.TS),
				})
				if err != nil {
					return fmt.Errorf("find imported Slackdump message: %w", err)
				}
				messageID := messageIDs[sourceMessageID(conversation.ID, message.TS)]
				if messageID == 0 {
					return fmt.Errorf("imported Slackdump message %s was not persisted", sourceMessageID(conversation.ID, message.TS))
				}
				if err = persistSlackdumpAttachments(
					scopedStore,
					export,
					*conversation,
					messageID,
					message,
					cc.opts.MediaConversation,
					opts,
					summary,
				); err != nil {
					return fmt.Errorf("persist Slackdump attachments for %s: %w", sourceMessageID(conversation.ID, message.TS), err)
				}
			}
			if opts.Limit > 0 && summary.MessagesProcessed-before >= opts.Limit {
				return errSlackdumpMessageLimitReached
			}
			return nil
		})
		if err != nil && !errors.Is(err, errSlackdumpMessageLimitReached) {
			return summary, err
		}
		summary.ConversationsProcessed++
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf(
				"conversation %d/%d (%s): %d messages",
				index+1,
				len(conversations),
				conversationTitle(conversation, importer.res.displayName),
				summary.MessagesProcessed-before,
			))
		}
	}

	if err = scopedStore.RecomputeConversationStats(source.ID); err != nil {
		return summary, err
	}
	if err = scopedStore.UpdateSyncCheckpoint(syncID, slackdumpCheckpoint(summary)); err != nil {
		return summary, err
	}
	if err = export.Close(); err != nil {
		return summary, fmt.Errorf("close Slackdump export %s: %w", inputPath, err)
	}
	exportClosed = true
	if err = scopedStore.CompleteSync(syncID, ""); err != nil {
		return summary, err
	}
	return summary, nil
}

func resolveSlackdumpIdentity(users []User, identity string) (User, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return User{}, errors.New("slackdump importing identity is required")
	}
	var matches []User
	for _, user := range users {
		if user.ID == identity {
			matches = append(matches, user)
		}
	}
	if len(matches) == 0 {
		for _, user := range users {
			if user.Profile.Email != "" && strings.EqualFold(user.Profile.Email, identity) {
				matches = append(matches, user)
			}
		}
	}
	switch len(matches) {
	case 0:
		return User{}, fmt.Errorf("slackdump identity %q not found in users.json", identity)
	case 1:
		return matches[0], nil
	default:
		return User{}, fmt.Errorf("slackdump identity %q matches multiple users in users.json", identity)
	}
}

func ensureSlackdumpMembership(
	st *store.Store,
	resolver *participantResolver,
	conversationID int64,
	conversation *Conversation,
	meUserID string,
	mediaPolicy attachmentpolicy.Policy,
) ([]messageRecipient, int, error) {
	userIDs := conversation.Members
	if conversation.IsIM {
		userIDs = []string{conversation.User, meUserID}
	} else if len(userIDs) == 0 {
		if conversation.NumMembers > 0 {
			if err := st.SetConversationMemberCount(conversationID, conversation.NumMembers); err != nil {
				return nil, 0, err
			}
			return nil, conversation.NumMembers, nil
		}
		if err := st.MarkConversationMemberCountUnknown(conversationID); err != nil {
			return nil, 0, err
		}
		return nil, unknownMembershipCount(mediaPolicy), nil
	}
	seen := make(map[string]struct{}, len(userIDs))
	participants := make([]store.ConversationParticipantRef, 0, len(userIDs))
	toRecipients := make([]messageRecipient, 0, len(userIDs))
	for _, userID := range userIDs {
		if userID == "" {
			continue
		}
		if _, duplicate := seen[userID]; duplicate {
			continue
		}
		seen[userID] = struct{}{}
		participantID, err := resolver.resolveID(userID)
		if err != nil {
			return nil, 0, err
		}
		if participantID == 0 {
			continue
		}
		participants = append(participants, store.ConversationParticipantRef{
			ParticipantID: participantID,
			Role:          "member",
		})
		if conversation.IsIM || conversation.IsMpim {
			toRecipients = append(toRecipients, messageRecipient{
				id:   participantID,
				name: resolver.displayName(userID),
			})
		}
	}
	if err := st.ReplaceConversationParticipants(conversationID, participants); err != nil {
		return nil, 0, err
	}
	participantCount := max(len(participants), conversation.NumMembers)
	if err := st.SetConversationMemberCount(conversationID, participantCount); err != nil {
		return nil, 0, err
	}
	return toRecipients, participantCount, nil
}

func slackdumpCheckpoint(summary *SlackdumpImportSummary) *store.Checkpoint {
	return &store.Checkpoint{
		MessagesProcessed: int64(summary.MessagesProcessed),
		MessagesAdded:     int64(summary.MessagesAdded),
		MessagesUpdated:   int64(summary.MessagesUpdated),
		ErrorsCount:       int64(summary.Errors),
	}
}

func persistSlackdumpAttachments(
	st *store.Store,
	dump *slackdumpExport,
	conversation Conversation,
	messageID int64,
	message *Message,
	mediaConversation attachmentpolicy.Conversation,
	opts SlackdumpImportOptions,
	summary *SlackdumpImportSummary,
) error {
	existing, err := st.MessageSlackAttachments(messageID)
	if err != nil {
		return fmt.Errorf("read existing attachments: %w", err)
	}
	if len(message.Files) == 0 && len(existing) == 0 {
		return nil
	}
	maxBytes := opts.MediaPolicy.MaxBytes
	if maxBytes <= 0 {
		maxBytes = opts.MaxMediaBytes
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	policy := opts.MediaPolicy
	policy.MaxBytes = maxBytes
	refs := make([]store.AttachmentRef, 0, len(message.Files))
	seen := make(map[string]struct{}, len(message.Files))
	for index := range message.Files {
		file := &message.Files[index]
		if file.ID == "" || file.Mode == "tombstone" {
			continue
		}
		sourceAttachmentID := slackAttachmentID(file.ID)
		seen[sourceAttachmentID] = struct{}{}
		if previous, ok := existing[sourceAttachmentID]; ok && previous.ContentHash != "" {
			previous.Role = store.AttachmentRoleStandalone
			previous.RoleSource = store.AttachmentRoleSourceProviderExplicit
			refs = append(refs, previous)
			continue
		}

		metadata := slackdumpMetadataAttachment(file, "slackdump:missing:"+file.ID)
		if reason := policy.Evaluate(mediaConversation, file.Size); reason != "" {
			metadata = slackdumpMetadataAttachment(file, "slackdump:skipped:"+file.ID)
			metadata.MediaType = ""
			metadata.State = attachmentpolicy.StateSkipped
			metadata.SkipReason = reason
			refs = append(refs, metadata)
			summary.AttachmentsSkipped++
			continue
		}
		content, found, readErr := dump.attachmentWithLimit(conversation, *file, maxBytes)
		if errors.Is(readErr, ErrAssetTooLarge) {
			metadata = slackdumpMetadataAttachment(file, "slackdump:skipped:"+file.ID)
			metadata.MediaType = ""
			metadata.State = attachmentpolicy.StateSkipped
			metadata.SkipReason = attachmentpolicy.SkipSizeCap
			refs = append(refs, metadata)
			summary.AttachmentsSkipped++
			continue
		}
		if readErr != nil {
			return readErr
		}
		if !found {
			refs = append(refs, metadata)
			summary.AttachmentsMissing++
			continue
		}
		if opts.AttachmentsDir == "" {
			metadata = slackdumpMetadataAttachment(file, "slackdump:available:"+file.ID)
			refs = append(refs, metadata)
			continue
		}

		attachment := &mime.Attachment{
			Filename:    file.Name,
			ContentType: file.Mimetype,
			Content:     content,
		}
		storagePath, err := attachmentexport.StoreAttachmentFileIncludingEmpty(opts.AttachmentsDir, attachment)
		if err != nil {
			return err
		}
		if storagePath == "" {
			refs = append(refs, metadata)
			summary.AttachmentsMissing++
			continue
		}
		refs = append(refs, store.AttachmentRef{
			Filename:           file.Name,
			MimeType:           file.Mimetype,
			StoragePath:        storagePath,
			ContentHash:        attachment.ContentHash,
			Size:               len(content),
			SourceAttachmentID: sourceAttachmentID,
			MediaType:          mediaTypeOf(file),
			Role:               store.AttachmentRoleStandalone,
			RoleSource:         store.AttachmentRoleSourceProviderExplicit,
			State:              attachmentpolicy.StateStored,
		})
		summary.AttachmentsDownloaded++
	}

	var omitted []string
	for sourceAttachmentID := range existing {
		if _, present := seen[sourceAttachmentID]; !present {
			omitted = append(omitted, sourceAttachmentID)
		}
	}
	sort.Strings(omitted)
	for _, sourceAttachmentID := range omitted {
		refs = append(refs, existing[sourceAttachmentID])
	}
	if err := st.ReplaceMessageSlackAttachments(messageID, refs); err != nil {
		return fmt.Errorf("replace attachment rows: %w", err)
	}
	if err := st.RecomputeMessageAttachmentStats(messageID); err != nil {
		return fmt.Errorf("recompute attachment stats: %w", err)
	}
	return nil
}

func slackdumpMetadataAttachment(file *File, fallbackPath string) store.AttachmentRef {
	storagePath := slackdumpMetadataURL(file)
	if storagePath == "" {
		storagePath = fallbackPath
	}
	return store.AttachmentRef{
		Filename:           file.Name,
		MimeType:           file.Mimetype,
		StoragePath:        storagePath,
		Size:               int(file.Size),
		SourceAttachmentID: slackAttachmentID(file.ID),
		MediaType:          "link",
		Role:               store.AttachmentRoleStandalone,
		RoleSource:         store.AttachmentRoleSourceProviderExplicit,
	}
}

func slackdumpMetadataURL(file *File) string {
	for _, candidate := range []string{file.Permalink, file.URLPrivate, file.URLPrivateDownload} {
		candidate = strings.TrimSpace(candidate)
		lower := strings.ToLower(candidate)
		if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") {
			return candidate
		}
	}
	return ""
}
