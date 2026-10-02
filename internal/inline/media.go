package inline

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

var r2Host = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9.-]*\.)?[a-f0-9]{32}(?:\.(?:eu|fedramp))?\.r2\.cloudflarestorage\.com$`)
var errMediaTooLarge = errors.New("inline media exceeds configured size cap")

func mediaURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return "", errors.New("Inline media requires a trusted HTTPS download URL")
	}
	host := strings.ToLower(u.Hostname())
	if !r2Host.MatchString(host) && (host != ProductionOrigin || u.Path != "/file") {
		return "", errors.New("Inline media host is not an allowed production origin")
	}
	return u.String(), nil
}

func (imp *Importer) download(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("Inline media download requires a positive size cap")
	}
	validated, err := mediaURL(rawURL)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, validated, nil)
	if err != nil {
		return nil, errors.New("construct Inline media request")
	}
	// Signed URLs carry their own limited authorization. Account headers,
	// cookies, and API transports must never be used by this downloader.
	client := &http.Client{Timeout: 10 * time.Minute, Transport: imp.mediaTransport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("Inline media redirects are refused")
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("Inline media download failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Inline media returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return nil, errMediaTooLarge
	}
	readLimit := maxBytes
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, readLimit))
	if err != nil {
		return nil, errors.New("read Inline media body")
	}
	if int64(len(data)) > maxBytes {
		return nil, errMediaTooLarge
	}
	return data, nil
}

func mediaType(media Media) string {
	switch {
	case media.Role == "photo" || strings.HasPrefix(media.ID, "photo:"):
		return "image"
	case media.Role == "video" || strings.HasPrefix(media.ID, "video:"):
		return "video"
	case media.Role == "voice" || strings.HasPrefix(media.ID, "voice:"):
		return "voice_note"
	case strings.HasPrefix(media.MIMEType, "image/"):
		return "image"
	case strings.HasPrefix(media.MIMEType, "video/"):
		return "video"
	case strings.HasPrefix(media.MIMEType, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

func mediaReference(media Media) (store.AttachmentRef, error) {
	if media.ID == "" || len(media.ID) > 256 || !validID(media.ChatID) || !validID(media.MessageID) || media.Size < 0 || media.Width < 0 || media.Height < 0 || media.DurationMS < 0 {
		return store.AttachmentRef{}, errors.New("invalid Inline media identity or metadata")
	}
	if uint64(media.Size) > uint64(^uint(0)>>1) {
		return store.AttachmentRef{}, errors.New("Inline media size exceeds archive representation")
	}
	metadata, err := json.Marshal(media, json.Deterministic(true))
	if err != nil {
		return store.AttachmentRef{}, err
	}
	return store.AttachmentRef{Filename: media.Filename, MimeType: media.MIMEType, StoragePath: "inline:pending:" + media.ID, SourceAttachmentID: "inline:" + media.ID, SourcePartKey: "inline:" + media.ID, Size: int(media.Size), Width: int64(media.Width), Height: int64(media.Height), DurationMS: int64(media.DurationMS), MediaType: mediaType(media), Metadata: string(metadata), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit, State: attachmentpolicy.StatePending}, nil
}

func refsSlice(refs map[string]store.AttachmentRef) []store.AttachmentRef {
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]store.AttachmentRef, 0, len(refs))
	for _, key := range keys {
		result = append(result, refs[key])
	}
	return result
}

func (imp *Importer) persistMedia(ctx context.Context, messageID int64, conversation Conversation, media []Media, opts ImportOptions, sum *ImportSummary) error {
	refs, err := imp.store.MessageInlineProviderAttachments(messageID)
	if err != nil {
		return err
	}
	if len(media) == 0 {
		return nil
	}
	policy := opts.MediaPolicy
	if policy.MaxBytes == 0 {
		policy.MaxBytes = attachmentpolicy.DefaultChatMaxBytes
	}
	policyContext := attachmentpolicy.Conversation{Type: conversationType(conversation.Type), ParticipantCount: conversation.MemberCount}
	evaluate := func(size int64) attachmentpolicy.SkipReason {
		if reason := policy.Evaluate(policyContext, 0); reason != "" {
			return reason
		}
		if policy.MaxParticipants > 0 && !conversation.MemberCountKnown {
			return attachmentpolicy.SkipParticipantThreshold
		}
		return policy.Evaluate(policyContext, size)
	}
	eligible := map[string]Media{}
	seen := map[string]bool{}
	var providerMessageID int64
	for _, item := range media {
		if item.ChatID != conversation.ID {
			return errors.New("Inline media belongs to a different chat")
		}
		if providerMessageID != 0 && item.MessageID != providerMessageID {
			return errors.New("Inline media crosses message identities")
		}
		providerMessageID = item.MessageID
		ref, refErr := mediaReference(item)
		if refErr != nil {
			return refErr
		}
		key := ref.SourceAttachmentID
		if seen[key] {
			return errors.New("Inline media occurrence is repeated")
		}
		seen[key] = true
		if prior, ok := refs[key]; ok {
			if prior.ContentHash != "" {
				continue
			}
			if prior.State == attachmentpolicy.StateUnavailable {
				continue
			}
			if prior.Size > ref.Size {
				ref.Size = prior.Size
			}
		}
		if reason := evaluate(int64(ref.Size)); reason != "" {
			ref.State = attachmentpolicy.StateSkipped
			ref.SkipReason = reason
			sum.AttachmentsSkipped++
		} else if opts.NoMedia {
			sum.AttachmentsPending++
		} else {
			eligible[key] = item
		}
		refs[key] = ref
	}
	// Metadata-only markers are durable before refresh/download, so a crash or
	// transient provider failure can always be repaired by media backfill.
	if err = imp.store.ReplaceMessageInlineProviderAttachments(messageID, refsSlice(refs)); err != nil {
		return err
	}
	if len(eligible) == 0 {
		return nil
	}
	refreshed, refreshErr := imp.client.Files(ctx, conversation.ID, []int64{providerMessageID})
	if refreshErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for key := range eligible {
			ref := refs[key]
			ref.State = attachmentpolicy.StateFailed
			ref.SkipReason = attachmentpolicy.SkipFetchFailure
			refs[key] = ref
			sum.AttachmentsPending++
			sum.Errors++
		}
		return imp.store.ReplaceMessageInlineProviderAttachments(messageID, refsSlice(refs))
	}
	fresh := map[string]Media{}
	for _, item := range refreshed {
		key := "inline:" + item.ID
		if item.ChatID != conversation.ID || item.MessageID != providerMessageID || !seen[key] || fresh[key].ID != "" {
			return errors.New("Inline file refresh does not match archived message media")
		}
		fresh[key] = item
	}
	for key := range eligible {
		if item, ok := fresh[key]; ok {
			ref, refErr := mediaReference(item)
			if refErr != nil {
				return refErr
			}
			if refs[key].Size > ref.Size {
				ref.Size = refs[key].Size
			}
			refs[key] = ref
		}
	}
	if err = imp.store.ReplaceMessageInlineProviderAttachments(messageID, refsSlice(refs)); err != nil {
		return err
	}
	for key := range eligible {
		if err = ctx.Err(); err != nil {
			return err
		}
		ref := refs[key]
		item, available := fresh[key]
		if !available || item.URL == "" {
			ref.State = attachmentpolicy.StateFailed
			ref.SkipReason = attachmentpolicy.SkipFetchFailure
			sum.AttachmentsPending++
			sum.Errors++
			refs[key] = ref
			continue
		}
		if reason := evaluate(int64(ref.Size)); reason != "" {
			ref.State = attachmentpolicy.StateSkipped
			ref.SkipReason = reason
			sum.AttachmentsSkipped++
			refs[key] = ref
			continue
		}
		data, downloadErr := imp.download(ctx, item.URL, policy.MaxBytes)
		if downloadErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(downloadErr, errMediaTooLarge) {
				ref.State = attachmentpolicy.StateSkipped
				ref.SkipReason = attachmentpolicy.SkipSizeCap
				ref.Size = attachmentpolicy.OversizeMarkerSize(policy.MaxBytes, int64(ref.Size))
				sum.AttachmentsSkipped++
			} else {
				ref.State = attachmentpolicy.StateFailed
				ref.SkipReason = attachmentpolicy.SkipFetchFailure
				sum.AttachmentsPending++
				sum.Errors++
			}
			refs[key] = ref
			continue
		}
		attachment := &mime.Attachment{Filename: item.Filename, ContentType: item.MIMEType, Content: data}
		if attachment.ContentType == "" {
			attachment.ContentType = http.DetectContentType(data)
			ref.MimeType = attachment.ContentType
		}
		path, storeErr := export.StoreAttachmentFileIncludingEmpty(opts.AttachmentsDir, attachment)
		if storeErr != nil {
			return fmt.Errorf("store Inline media: %w", storeErr)
		}
		if path == "" {
			return errors.New("Inline attachment storage is unavailable")
		}
		ref.StoragePath = path
		ref.ContentHash = attachment.ContentHash
		ref.Size = len(data)
		ref.State = attachmentpolicy.StateStored
		ref.SkipReason = ""
		refs[key] = ref
		sum.AttachmentsDownloaded++
		if err = imp.store.ReplaceMessageInlineProviderAttachments(messageID, refsSlice(refs)); err != nil {
			return err
		}
	}
	return imp.store.ReplaceMessageInlineProviderAttachments(messageID, refsSlice(refs))
}

// BackfillMedia refreshes signed URLs only for selected chats and retries their
// durable unfinished occurrences. It never broadens the archived chat scope.
func (imp *Importer) BackfillMedia(ctx context.Context, opts ImportOptions) (sum *ImportSummary, err error) {
	started := time.Now()
	sum = &ImportSummary{}
	defer func() { sum.Duration = time.Since(started) }()
	opts, err = imp.validateOptions(ctx, opts)
	if err != nil {
		return sum, err
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Account.Identifier())
	if err != nil {
		return sum, err
	}
	sum.SourceID = source.ID
	runID, err := imp.store.StartSyncContext(ctx, source.ID, "inline_media")
	if err != nil {
		return sum, err
	}
	scoped := &Importer{store: imp.store.ScopedToSync(source.ID, runID), client: imp.client, mediaTransport: imp.mediaTransport}
	defer func() {
		if err != nil {
			err = errors.Join(err, scoped.store.FailSync(runID, "Inline media backfill did not complete"))
		}
	}()
	// Media work must preserve the latest message-sync resume blob unchanged.
	state, err := imp.loadState(source.ID, opts.Account.Identifier())
	if err != nil {
		return sum, err
	}
	selected := map[int64]bool{}
	for _, id := range opts.ChatIDs {
		selected[id] = true
	}
	conversations := map[int64]Conversation{}
	for _, chatID := range opts.ChatIDs {
		conversation, readErr := scoped.client.Conversation(ctx, chatID)
		if readErr != nil {
			return sum, readErr
		}
		if conversation.ID != chatID {
			return sum, errors.New("Inline backfill chat response does not match selection")
		}
		if _, err = scoped.persistConversation(source.ID, conversation); err != nil {
			return sum, err
		}
		conversations[chatID] = conversation
	}
	pending, err := scoped.store.ListInlineProviderRetryableAttachmentMessages(source.ID, opts.MediaPolicy)
	if err != nil {
		return sum, err
	}
	for _, row := range pending {
		if err = ctx.Err(); err != nil {
			return sum, err
		}
		metadataValue, readErr := scoped.store.GetMessageMetadata(row.MessageID)
		if readErr != nil {
			return sum, readErr
		}
		var metadata messageMetadata
		if err = json.Unmarshal([]byte(metadataValue.String), &metadata); err != nil {
			return sum, err
		}
		if !selected[metadata.ChatID] {
			continue
		}
		if opts.Limit > 0 && sum.MessagesProcessed >= opts.Limit {
			break
		}
		conversation := conversations[metadata.ChatID]
		opts.NoMedia = false
		if err = scoped.persistMedia(ctx, row.MessageID, conversation, metadata.Media, opts, sum); err != nil {
			return sum, err
		}
		sum.MessagesProcessed++
	}
	var blob string
	blob, err = state.Marshal()
	if err != nil {
		return sum, err
	}
	if err = scoped.store.UpdateSyncCheckpointContext(ctx, runID, &store.Checkpoint{PageToken: blob, MessagesProcessed: int64(sum.MessagesProcessed), ErrorsCount: int64(sum.Errors)}); err != nil {
		return sum, err
	}
	err = scoped.store.CompleteSyncContext(ctx, runID, blob)
	return sum, err
}
