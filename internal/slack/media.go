package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// defaultMaxMediaBytes caps individual file downloads (config max_media_mb
// overrides).
const defaultMaxMediaBytes = int64(100 << 20)

// mediaHost is the only host file bytes are ever fetched from with the
// bearer token. Message JSON is attacker-influenceable (any workspace member
// authors it), so fetching arbitrary url_private values would hand the token
// to whatever host the URL names — the Slack analogue of the Graph
// token-exfiltration finding on the Teams PR. Off-host files are recorded as
// metadata-only link rows instead.
const mediaHost = "files.slack.com"

// errOffHost reports a file URL that is not on files.slack.com.
var errOffHost = errors.New("file URL not on files.slack.com")

// slackAttachmentID namespaces Slack-managed attachment rows in
// attachments.source_attachment_id.
func slackAttachmentID(fileID string) string {
	return "slack:" + fileID
}

// mediaTypeOf maps a Slack file's mimetype to msgvault's attachments.media_type.
func mediaTypeOf(f *File) string {
	switch {
	case strings.HasPrefix(f.Mimetype, "image/"):
		return "image"
	case strings.HasPrefix(f.Mimetype, "video/"):
		return "video"
	case strings.HasPrefix(f.Mimetype, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

// fetchableURL validates a file's private URL for token-bearing download:
// https, exactly files.slack.com, no userinfo. Anything else is off-host.
func fetchableURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse file URL: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || !strings.EqualFold(u.Host, mediaHost) {
		return "", fmt.Errorf("%q: %w", rawURL, errOffHost)
	}
	return u.String(), nil
}

// DownloadFile fetches a files.slack.com URL with the bearer token, capped at
// maxBytes. Redirects are refused entirely: a redirect off-host would carry
// the token, and same-host redirects do not occur in practice.
func (c *Client) DownloadFile(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	fetchURL, err := fetchableURL(rawURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	client := &http.Client{
		Timeout: c.http.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("refusing redirect to %s: %w", req.URL.Host, errOffHost)
		},
	}
	if c.mediaTransport != nil {
		client.Transport = c.mediaTransport
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack file GET: status %d", resp.StatusCode)
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("slack file GET: %d bytes: %w", resp.ContentLength, ErrAssetTooLarge)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("slack file GET: read body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrAssetTooLarge
	}
	return data, nil
}

// persistFiles downloads a message's files into content-addressed storage
// and replaces the message's Slack attachment rows. Already-downloaded media
// (matched by source_attachment_id) is kept without re-fetching — including
// files the source has since tombstoned or dropped from the message
// (archive semantics: source deletions never propagate). External or
// off-host files become metadata-only link rows (media_type "link", the
// permalink as storage path); failed or over-cap downloads leave a pending
// marker row (no content hash) for BackfillMedia to retry.
//
// DOWNLOAD failures are non-fatal because they leave that durable marker;
// STORE failures are fatal and hold the cursor — a row write that fails
// leaves downloaded bytes orphaned in CAS with no marker, invisible to
// backfill forever, so the run must stop rather than advance past it.
func (imp *Importer) persistFiles(ctx context.Context, syncID, messageID int64, m *Message, opts ImportOptions, sum *ImportSummary) error {
	existing, err := imp.store.MessageSlackAttachments(messageID)
	if err != nil {
		return fmt.Errorf("read attachment rows: %w", err)
	}
	if len(m.Files) == 0 && len(existing) == 0 {
		return nil
	}
	maxBytes := opts.MaxMediaBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	refs := make([]store.AttachmentRef, 0, len(m.Files))
	seen := map[string]bool{}
	for i := range m.Files {
		f := &m.Files[i]
		if f.ID == "" || f.Mode == "tombstone" {
			continue // preserved below if a downloaded row already exists
		}
		sourceAttID := slackAttachmentID(f.ID)
		seen[sourceAttID] = true
		if prev, ok := existing[sourceAttID]; ok && prev.ContentHash != "" {
			refs = append(refs, prev)
			continue
		}
		linkRow := store.AttachmentRef{
			Filename:           f.Name,
			MimeType:           f.Mimetype,
			StoragePath:        f.Permalink,
			Size:               int(f.Size),
			SourceAttachmentID: sourceAttID,
			MediaType:          "link",
		}
		pendingRow := linkRow
		pendingRow.MediaType = ""

		// pend leaves a retryable marker; link records metadata permanently.
		pend := func(status, kind string, err error) {
			imp.recordItem(syncID, sourceMessageID("", m.TS), "attachment", status, kind, err)
			refs = append(refs, pendingRow)
			sum.AttachmentsPending++
			if status == store.SyncRunItemStatusError {
				sum.Errors++
			}
		}

		fetchRef := f.URLPrivate
		if fetchRef == "" {
			fetchRef = f.URLPrivateDownload
		}
		if _, herr := fetchableURL(fetchRef); herr != nil || f.IsExternal {
			// Off-host / external: never fetched with the token (see mediaHost).
			refs = append(refs, linkRow)
			continue
		}
		if opts.NoMedia || opts.AttachmentsDir == "" {
			// Deferred, not declined: a pending marker keeps the file
			// discoverable by backfill-slack-media once media is enabled.
			// A link row here would hide it from the pending queries
			// forever (link rows are reserved for genuinely external
			// files that must never be fetched with the token).
			refs = append(refs, pendingRow)
			sum.AttachmentsPending++
			continue
		}
		if f.Size > 0 && f.Size > maxBytes {
			pend(store.SyncRunItemStatusSkipped, "slack_media_too_large",
				fmt.Errorf("file %s is %d bytes (cap %d)", f.ID, f.Size, maxBytes))
			continue
		}
		data, derr := imp.client.DownloadFile(ctx, fetchRef, maxBytes)
		if errors.Is(derr, ErrAssetTooLarge) {
			pend(store.SyncRunItemStatusSkipped, "slack_media_too_large", derr)
			continue
		}
		if derr != nil {
			if ctx.Err() != nil {
				// Interrupted: keep the pending marker without charging an error.
				refs = append(refs, pendingRow)
				sum.AttachmentsPending++
				continue
			}
			pend(store.SyncRunItemStatusError, "slack_media_error", derr)
			continue
		}
		ma := &mime.Attachment{Filename: f.Name, ContentType: f.Mimetype, Content: data}
		storagePath, serr := export.StoreAttachmentFile(opts.AttachmentsDir, ma)
		if serr != nil || storagePath == "" {
			pend(store.SyncRunItemStatusError, "slack_media_error", serr)
			continue
		}
		stored := store.AttachmentRef{
			Filename:           f.Name,
			MimeType:           f.Mimetype,
			StoragePath:        storagePath,
			ContentHash:        ma.ContentHash,
			Size:               len(data),
			SourceAttachmentID: sourceAttID,
			MediaType:          mediaTypeOf(f),
		}
		refs = append(refs, stored)
		sum.AttachmentsDownloaded++
	}
	// Files tombstoned or omitted at the source keep their archived rows —
	// downloaded media AND metadata-only link rows (all the record we will
	// ever have for an external file): a Slack-side deletion, or an edit
	// that drops a file, must never reach into the archive. Only stale
	// pending markers clear: a gone file can never be fetched, and keeping
	// its marker would wedge the pending queue forever.
	var keep []string
	for id, prev := range existing {
		if (prev.ContentHash != "" || prev.MediaType == "link") && !seen[id] {
			keep = append(keep, id)
		}
	}
	sort.Strings(keep)
	for _, id := range keep {
		refs = append(refs, existing[id])
	}
	if err := imp.store.ReplaceMessageSlackAttachments(messageID, refs); err != nil {
		return fmt.Errorf("replace attachment rows: %w", err)
	}
	if err := imp.store.RecomputeMessageAttachmentStats(messageID); err != nil {
		return fmt.Errorf("recompute attachment stats: %w", err)
	}
	return nil
}
