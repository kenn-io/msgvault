package matrix

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func (imp *Importer) selectMediaVersion(ctx context.Context, messageID int64, body string, content *event.MessageEventContent, eventType event.Type, metadata []byte, edited bool, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	var download func() error
	worker := *imp
	if err := imp.mediaTransaction(ctx, opts, func(s *store.Store) error {
		worker.store = s
		if err := worker.setBody(messageID, body); err != nil {
			return err
		}
		if content.MsgType.IsMedia() {
			if err := worker.prepareMedia(ctx, messageID, content, eventType, conversation, opts, sum, &download); err != nil {
				return err
			}
		} else if err := worker.applyMedia(ctx, messageID, content, eventType, conversation, pendingMediaOptions(opts), sum); err != nil {
			return err
		}
		if err := s.RecomputeMessageSizeEstimateContext(ctx, messageID); err != nil {
			return err
		}
		if err := s.SetMessageEditedState(messageID, edited); err != nil {
			return err
		}
		return s.SetMessageMetadata(messageID, sql.NullString{String: string(metadata), Valid: true})
	}); err != nil {
		return err
	}
	worker.store = imp.store
	if download != nil {
		return download()
	}
	return nil
}

// Stickers omit msgtype, so downloads use image content while the occurrence keeps its sticker role.
func mediaContent(evt *event.Event) *event.MessageEventContent {
	content := evt.Content.AsMessage()
	if evt.Type != event.EventSticker {
		return content
	}
	image := *content
	if image.MsgType == "" {
		image.MsgType = event.MsgImage
	}
	if image.NewContent != nil && image.NewContent.MsgType == "" {
		replacement := *image.NewContent
		replacement.MsgType = event.MsgImage
		image.NewContent = &replacement
	}
	return &image
}

func pendingMediaOptions(opts ImportOptions) ImportOptions {
	opts.NoMedia = true
	opts.MediaMutation = nil
	return opts
}

// Count archived message rows, including text-only messages, rather than media downloads.
const legacyMediaDiscoveryBudget = 10_000

func (imp *Importer) discoverLegacyMedia(ctx context.Context, sourceID int64, state *SyncState, opts ImportOptions) (bool, error) {
	after := state.MediaDiscoveryAfter
	complete := !state.MediaDiscoveryIncomplete
	for scanned := 0; scanned < legacyMediaDiscoveryBudget; {
		items, err := imp.store.ListMatrixLegacyMediaMessagesContext(ctx, sourceID, after, opts.Rooms, opts.ExcludeRooms, opts.MediaPolicy)
		if err != nil {
			return false, err
		}
		if len(items) == 0 {
			state.MediaDiscoveryAfter, state.MediaDiscoveryIncomplete = 0, false
			return complete, nil
		}
		for _, item := range items {
			after = item.MessageID
			scanned++
			archived, content, selected, err := imp.archivedMediaContent(item.MessageID)
			if err != nil {
				if errors.Is(err, errUnsupportedMediaContentVersion) || errors.Is(err, errReadMediaMetadata) || errors.Is(err, errReadArchivedEvent) {
					return false, err
				}
				slog.Warn("skip unreadable Matrix media event", "message_id", item.MessageID, "error", err)
				continue
			}
			if archived.Type != event.EventMessage && archived.Type != event.EventSticker {
				continue
			}
			if !content.MsgType.IsMedia() && selected.EventID == "" {
				continue
			}
			if selected.Content == nil && selected.EventID != "" {
				// Older archives saved only the selected edit ID, so its content needs one lookup.
				edit, err := imp.runtime.Client.GetEvent(ctx, id.RoomID(item.ChatID), id.EventID(selected.EventID))
				if err != nil {
					if ctx.Err() != nil {
						return false, ctx.Err()
					}
					var httpErr mautrix.HTTPError
					if errors.Is(err, mautrix.MUnknownToken) || (errors.As(err, &httpErr) && httpErr.IsStatus(http.StatusUnauthorized)) {
						return false, fmt.Errorf("authenticate legacy Matrix edit lookup: %w", err)
					}
					slog.Warn("skip unavailable legacy Matrix edit", "message_id", item.MessageID, "error", err)
					if temporaryMediaError(err) {
						complete = false
					}
					continue
				}
				if edit.Type == event.EventEncrypted {
					if !parseContent(edit) || edit.Content.AsEncrypted().Algorithm != id.AlgorithmMegolmV1 {
						continue
					}
					edit, err = imp.runtime.decrypt(ctx, edit)
					if err != nil {
						complete = false
						continue
					}
				}
				if !parseContent(edit) || !validReplacement(archived, edit) || edit.Content.AsMessage().RelatesTo.GetReplaceID() != archived.ID {
					continue
				}
				content = mediaContent(edit).NewContent
				if content == nil {
					continue
				}
				saved, err := imp.mergedMetadata(item.MessageID, map[string]any{"matrix_media_content": content, "matrix_media_content_version": 1})
				if err != nil {
					return false, err
				}
				if err := mediaMutation(ctx, opts, func() error {
					return imp.store.SetMessageMetadataContext(ctx, item.MessageID, sql.NullString{String: string(saved), Valid: true})
				}); err != nil {
					return false, err
				}
			}
			if !content.MsgType.IsMedia() {
				continue
			}
			conversation := attachmentpolicy.Conversation{Type: item.ConversationType, ParticipantCount: item.ParticipantCount}
			pending := pendingMediaOptions(opts)
			pending.MediaMutation = opts.MediaMutation
			if err := imp.applyMedia(ctx, item.MessageID, content, archived.Type, conversation, pending, &ImportSummary{}); err != nil {
				return false, err
			}
		}
		state.MediaDiscoveryAfter, state.MediaDiscoveryIncomplete = after, !complete
	}
	return false, nil
}

func mediaDiscoveryScope(opts ImportOptions) string {
	if len(opts.Rooms) == 0 && len(opts.ExcludeRooms) == 0 {
		return ""
	}
	filters := [2][]string{slices.Clone(opts.Rooms), slices.Clone(opts.ExcludeRooms)}
	for i := range filters {
		slices.Sort(filters[i])
		filters[i] = slices.Compact(filters[i])
	}
	encoded, _ := json.Marshal(filters)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
}

// applyMedia reconciles attachments and estimates with the selected content.
func (imp *Importer) applyMedia(ctx context.Context, messageID int64, content *event.MessageEventContent, eventType event.Type, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	if content.MsgType.IsMedia() {
		if err := imp.persistMedia(ctx, messageID, content, eventType, conversation, opts, sum); err != nil {
			return err
		}
	} else {
		return mediaMutation(ctx, opts, func() error {
			existing, err := imp.existingMediaAttachments(messageID)
			if err != nil || len(existing) == 0 {
				return err
			}
			return imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, nil)
		})
	}
	return nil
}

func (imp *Importer) retryArchivedMedia(ctx context.Context, sourceID int64, opts ImportOptions, sum *ImportSummary) error {
	var after int64
	for {
		items, err := imp.store.ListMatrixRetryableAttachmentMessagesContext(ctx, sourceID, after, 100, opts.MediaPolicy)
		if err != nil || len(items) == 0 {
			return err
		}
		for _, item := range items {
			after = item.MessageID
			if !roomIncluded(item.ChatID, opts.Rooms, opts.ExcludeRooms) {
				continue
			}
			refs, err := imp.existingMediaAttachments(item.MessageID)
			if err != nil {
				return err
			}
			conversation := attachmentpolicy.Conversation{Type: item.ConversationType, ParticipantCount: item.ParticipantCount}
			excluded, backoff := false, !opts.Full && len(refs) > 0
			for _, ref := range refs {
				excluded = excluded || opts.MediaPolicy.Evaluate(store.MediaConversation(conversation), int64(ref.Size)) != ""
				var metadata matrixMediaMetadata
				backoff = backoff &&
					(ref.State == attachmentpolicy.StatePending || ref.State == attachmentpolicy.StateFailed) &&
					json.Unmarshal([]byte(ref.Metadata), &metadata) == nil && time.Now().Before(metadata.RetryAfter)
			}
			if !excluded && (opts.NoMedia || opts.AttachmentsDir == "" || backoff) {
				continue
			}
			archived, content, _, err := imp.archivedMediaContent(item.MessageID)
			if err != nil {
				if errors.Is(err, errUnsupportedMediaContentVersion) || errors.Is(err, errReadMediaMetadata) || errors.Is(err, errReadArchivedEvent) {
					return err
				}
				slog.Warn("skip unreadable Matrix media event", "message_id", item.MessageID, "error", err)
				continue
			}
			if err := imp.applyMedia(ctx, item.MessageID, content, archived.Type, conversation, opts, sum); err != nil {
				return err
			}
		}
	}
}

type selectedMediaContent struct {
	appliedEdit

	Content *event.MessageEventContent `json:"matrix_media_content"`
}

var errReadMediaMetadata = errors.New("read Matrix media metadata")

func (imp *Importer) archivedMediaContent(messageID int64) (*event.Event, *event.MessageEventContent, selectedMediaContent, error) {
	var selected selectedMediaContent
	archived, err := imp.archivedEvent(messageID)
	if err != nil {
		return nil, nil, selected, err
	}
	metadata, err := imp.store.GetMessageMetadata(messageID)
	if err != nil {
		return nil, nil, selected, fmt.Errorf("%w: %w", errReadMediaMetadata, err)
	}
	if metadata.Valid {
		if err := checkMediaContentVersion([]byte(metadata.String)); err != nil {
			return nil, nil, selected, err
		}
		if err := json.Unmarshal([]byte(metadata.String), &selected); err != nil {
			return nil, nil, selected, err
		}
	}
	content := mediaContent(archived)
	if selected.Content != nil {
		content = selected.Content
	}
	return archived, content, selected, nil
}

func (imp *Importer) existingMediaAttachments(messageID int64) (map[string]store.AttachmentRef, error) {
	existing, err := imp.store.MessageMatrixAttachments(messageID)
	if err != nil {
		return nil, err
	}
	for _, ref := range existing {
		var metadata matrixMediaMetadata
		if json.Unmarshal([]byte(ref.Metadata), &metadata) == nil && strings.HasPrefix(metadata.Input, "sha256:v") && !strings.HasPrefix(metadata.Input, "sha256:v1:") {
			return nil, fmt.Errorf("unsupported Matrix media fingerprint format %q", metadata.Input)
		}
	}
	return existing, nil
}

func mediaMutation(ctx context.Context, opts ImportOptions, write func() error) error {
	if opts.MediaMutation != nil {
		return opts.MediaMutation(ctx, write)
	}
	return write()
}

func (imp *Importer) mediaTransaction(ctx context.Context, opts ImportOptions, write func(*store.Store) error) error {
	return mediaMutation(ctx, opts, func() error {
		return imp.store.MatrixRecoveryTransaction(ctx, write)
	})
}

func (imp *Importer) persistMedia(ctx context.Context, messageID int64, content *event.MessageEventContent, eventType event.Type, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	var download func() error
	if err := mediaMutation(ctx, opts, func() error {
		return imp.prepareMedia(ctx, messageID, content, eventType, conversation, opts, sum, &download)
	}); err != nil {
		return err
	}
	if download != nil {
		return download()
	}
	return nil
}

func (imp *Importer) prepareMedia(ctx context.Context, messageID int64, content *event.MessageEventContent, eventType event.Type, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary, download *func() error) error {
	existing, err := imp.existingMediaAttachments(messageID)
	if err != nil {
		return err
	}
	uri := content.URL
	if content.File != nil {
		uri = content.File.URL
	}
	if uri == "" {
		if len(existing) == 0 {
			return nil
		}
		return imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, nil)
	}
	input := matrixMediaInput{URL: string(uri)}
	if content.File != nil {
		input.Key, input.IV = content.File.Key.Key, content.File.InitVector
		input.SHA256, input.Version = content.File.Hashes.SHA256, content.File.Version
		if content.File.Key.Algorithm != "A256CTR" {
			input.Algorithm = &content.File.Key.Algorithm
		}
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("sha256:v1:%x", sha256.Sum256(canonical))
	metadata := matrixMediaMetadata{Input: digest}
	var size int64
	var mimeType string
	if content.Info != nil {
		size = max(int64(content.Info.Size), 0)
		mimeType = content.Info.MimeType
	}
	ref := store.AttachmentRef{
		Filename: content.GetFileName(), MimeType: mimeType, Size: int(size),
		SourceAttachmentID: "matrix:" + string(uri), MediaType: strings.TrimPrefix(string(content.MsgType), "m."),
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit,
		State: attachmentpolicy.StatePending,
	}
	ref.StoragePath = ref.SourceAttachmentID
	ref.SourcePartKey = ref.SourceAttachmentID
	if eventType == event.EventSticker {
		ref.Role = store.AttachmentRoleSticker
	}
	archived, seen := existing[ref.SourceAttachmentID]
	var previous matrixMediaMetadata
	seen = seen && json.Unmarshal([]byte(archived.Metadata), &previous) == nil
	seen = seen && previous.Input == digest
	replace := func() error {
		stored := archived
		if previous.Input == metadata.Input && previous.Attempts == metadata.Attempts && previous.RetryAfter.Equal(metadata.RetryAfter) {
			stored.Metadata = ref.Metadata
		}
		if len(existing) == 1 && stored == ref {
			return nil
		}
		return imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, []store.AttachmentRef{ref})
	}
	if seen {
		if ref.MimeType == "" {
			ref.MimeType = archived.MimeType
		}
		metadata.Attempts = previous.Attempts
		metadata.RetryAfter = previous.RetryAfter
		if archived.State == attachmentpolicy.StatePending {
			ref.SkipReason = archived.SkipReason
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	ref.Metadata = string(encoded)
	if seen && int64(archived.Size) > size {
		size = int64(archived.Size)
		ref.Size = archived.Size
	}
	if seen && archived.ContentHash != "" && archived.StoragePath != "" {
		// Empty encrypted fields can share a plain-media fingerprint.
		if content.File != nil {
			if err := content.File.PrepareForDecryption(); err != nil {
				return imp.persistMediaFailure(ctx, messageID, ref, metadata, pendingMediaOptions(opts))
			}
		}
		ref.StoragePath = archived.StoragePath
		ref.ContentHash = archived.ContentHash
		ref.Size = archived.Size
		ref.State, ref.SkipReason = attachmentpolicy.StateStored, ""
		return replace()
	}
	if seen && archived.State == attachmentpolicy.StateFailed {
		ref.State, ref.SkipReason = attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure
	}
	if reason := opts.MediaPolicy.Evaluate(store.MediaConversation(conversation), size); reason != "" {
		ref.State, ref.SkipReason = attachmentpolicy.StateSkipped, reason
		return replace()
	}
	if opts.NoMedia || opts.AttachmentsDir == "" || *imp.mediaUnavailable {
		return replace()
	}
	attempt := fmt.Sprintf("%d:%s", messageID, digest)
	if imp.mediaAttempted[attempt] {
		return replace()
	}
	if !opts.Full && time.Now().Before(metadata.RetryAfter) {
		return replace()
	}
	ref.State, ref.SkipReason = attachmentpolicy.StatePending, ""
	if err := replace(); err != nil {
		return err
	}
	imp.mediaAttempted[attempt] = true
	*download = func() error {
		return imp.downloadMedia(ctx, messageID, content, ref, metadata, opts, sum)
	}
	return nil
}

func validMediaServerName(server string) bool {
	if server == "." || server == ".." || !id.ValidateServerName(server) {
		return false
	}
	if strings.HasPrefix(server, "[") {
		address, err := netip.ParseAddr(server[1:strings.IndexByte(server, ']')])
		return err == nil && address.Is6()
	}
	return true
}

func temporaryMediaError(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Response == nil {
		return true
	}
	status := httpErr.Response.StatusCode
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func (imp *Importer) downloadMedia(ctx context.Context, messageID int64, content *event.MessageEventContent, ref store.AttachmentRef, metadata matrixMediaMetadata, opts ImportOptions, sum *ImportSummary) error {
	uri := content.URL
	if content.File != nil {
		if err := content.File.PrepareForDecryption(); err != nil {
			return imp.persistMediaFailure(ctx, messageID, ref, metadata, opts)
		}
		uri = content.File.URL
	}
	parsed, err := uri.Parse()
	if err != nil || !validMediaServerName(parsed.Homeserver) || !id.IsValidMediaID(parsed.FileID) {
		return imp.persistMediaFailure(ctx, messageID, ref, metadata, opts)
	}
	limit := opts.MediaPolicy.MaxBytes
	if limit <= 0 {
		limit = attachmentpolicy.DefaultChatMaxBytes
	}
	downloadCtx, cancel := context.WithTimeout(ctx, attachmentpolicy.DownloadTimeout(limit))
	defer cancel()
	httpClient := *imp.runtime.Client.Client
	// The request context bounds the entire transfer, including body reads.
	httpClient.Timeout = 0
	_, resp, err := imp.runtime.Client.MakeFullRequestWithResp(downloadCtx, mautrix.FullRequest{
		Method: http.MethodGet, URL: imp.runtime.Client.BuildClientURL("v1", "media", "download", parsed.Homeserver, parsed.FileID),
		DontReadResponse: true, Client: &httpClient,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var httpErr mautrix.HTTPError
		if errors.As(err, &httpErr) && httpErr.IsStatus(http.StatusNotFound) && errors.Is(err, mautrix.MUnrecognized) {
			*imp.mediaUnavailable = true
			slog.Warn("Matrix homeserver lacks authenticated media downloads; media stays pending for this run")
			return nil
		}
		if errors.Is(err, mautrix.MUnknownToken) || (errors.As(err, &httpErr) && httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusUnauthorized) {
			return fmt.Errorf("authenticate Matrix media download: %w", err)
		}
		if !temporaryMediaError(err) {
			return imp.persistMediaFailure(ctx, messageID, ref, metadata, opts)
		}
		return imp.persistMediaTransientFailure(ctx, messageID, ref, metadata, opts)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength > limit {
		*imp.mediaTransientFailures = 0
		ref.State, ref.SkipReason, ref.Size = attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap, attachmentpolicy.OversizeMarkerSize(limit, resp.ContentLength)
		return mediaMutation(ctx, opts, func() error {
			return imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, []store.AttachmentRef{ref})
		})
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return imp.persistMediaTransientFailure(ctx, messageID, ref, metadata, opts)
	}
	*imp.mediaTransientFailures = 0
	if int64(len(data)) > limit {
		ref.State, ref.SkipReason, ref.Size = attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap, attachmentpolicy.OversizeMarkerSize(limit, int64(len(data)))
		return mediaMutation(ctx, opts, func() error {
			return imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, []store.AttachmentRef{ref})
		})
	}
	if content.File != nil {
		if err := content.File.DecryptInPlace(data); err != nil {
			return imp.persistMediaFailure(ctx, messageID, ref, metadata, opts)
		}
	}
	if ref.MimeType == "" {
		ref.MimeType = http.DetectContentType(data)
	}
	write := func() error {
		attachment := &msgmime.Attachment{Filename: ref.Filename, ContentType: ref.MimeType, Content: data}
		storagePath, err := export.StoreAttachmentFileIncludingEmpty(opts.AttachmentsDir, attachment)
		if err != nil {
			return fmt.Errorf("store Matrix media: %w", err)
		}
		ref.StoragePath, ref.ContentHash, ref.Size = storagePath, attachment.ContentHash, len(data)
		ref.State, ref.SkipReason = attachmentpolicy.StateStored, ""
		if err := imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, []store.AttachmentRef{ref}); err != nil {
			return err
		}
		sum.AttachmentsDownloaded++
		return nil
	}
	return mediaMutation(ctx, opts, write)
}

type matrixMediaInput struct {
	URL       string  `json:"url"`
	Key       string  `json:"k,omitempty"`
	IV        string  `json:"iv,omitempty"`
	SHA256    string  `json:"sha256,omitempty"`
	Version   string  `json:"v,omitempty"`
	Algorithm *string `json:"alg,omitempty"`
}

var errUnsupportedMediaContentVersion = errors.New("unsupported Matrix media content version")

func checkMediaContentVersion(data []byte) error {
	var saved struct {
		Version int `json:"matrix_media_content_version"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	if saved.Version > 1 {
		return fmt.Errorf("%w %d", errUnsupportedMediaContentVersion, saved.Version)
	}
	return nil
}

type matrixMediaMetadata struct {
	Input      string    `json:"matrix_media_input"`
	Attempts   int       `json:"transient_attempts,omitempty"`
	RetryAfter time.Time `json:"retry_after,omitzero"`
}

func (imp *Importer) persistMediaTransientFailure(ctx context.Context, messageID int64, ref store.AttachmentRef, metadata matrixMediaMetadata, opts ImportOptions) error {
	*imp.mediaTransientFailures++
	if *imp.mediaTransientFailures >= 3 {
		*imp.mediaUnavailable = true
		slog.Warn("Matrix media downloads stopped after three consecutive transient failures; media stays pending for this run")
	}
	metadata.Attempts++
	backoff := min(time.Hour<<min(max(metadata.Attempts-1, 0), 5), 24*time.Hour)
	metadata.RetryAfter = time.Now().Add(backoff)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	ref.Metadata, ref.SkipReason = string(encoded), attachmentpolicy.SkipFetchFailure
	ref.State = attachmentpolicy.StatePending
	return mediaMutation(ctx, opts, func() error {
		return imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, []store.AttachmentRef{ref})
	})
}

func (imp *Importer) persistMediaFailure(ctx context.Context, messageID int64, ref store.AttachmentRef, metadata matrixMediaMetadata, opts ImportOptions) error {
	*imp.mediaTransientFailures = 0
	metadata.RetryAfter = time.Now().Add(7 * 24 * time.Hour)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	ref.Metadata = string(encoded)
	ref.State, ref.SkipReason = attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure
	return mediaMutation(ctx, opts, func() error {
		return imp.store.ReplaceMessageMatrixAttachments(ctx, messageID, []store.AttachmentRef{ref})
	})
}
