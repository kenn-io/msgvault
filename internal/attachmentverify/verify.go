// Package attachmentverify relates a bounded selection of live attachment
// references to the existing physical readers. It never repairs storage.
package attachmentverify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const (
	Verified         = "verified"
	MissingLoose     = "missing_loose"
	PackUnavailable  = "pack_unavailable"
	Corrupt          = "corrupt"
	MetadataMismatch = "metadata_mismatch"
	Omitted          = "intentionally_omitted"
	NotMaterialized  = "not_materialized"
	Inconclusive     = "inconclusive"
	NotChecked       = "not_checked"
)

// Options bounds selection, physical reads and runtime. The page counts
// attachment rows; each has one content slot and an optional thumbnail slot.
type Options struct {
	AfterID  int64
	Limit    int
	MaxBytes int64
	Timeout  time.Duration
}

// Scope records the requested bounds using transport-friendly values.
type Scope struct {
	AfterID  int64  `json:"after_id"`
	Limit    int    `json:"limit"`
	MaxBytes int64  `json:"max_bytes"`
	Timeout  string `json:"timeout"`
}

func (o Options) Validate() error {
	if o.AfterID < 0 || o.Limit < 1 || o.Limit > 1000 {
		return errors.New("after-id must be nonnegative and limit must be between 1 and 1000")
	}
	if o.MaxBytes < 1 || o.MaxBytes > 1<<40 {
		return errors.New("max-bytes must be between 1 and 1099511627776")
	}
	if o.Timeout < time.Second || o.Timeout > time.Hour {
		return errors.New("timeout must be between 1s and 1h")
	}
	return nil
}

// StreamStore is the production mixed loose/packed streaming contract.
type StreamStore interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

type Reference struct {
	AttachmentID int64  `json:"attachment_id"`
	MessageID    int64  `json:"message_id"`
	Kind         string `json:"kind"`
	Hash         string `json:"hash,omitempty"`
	ExpectedSize *int64 `json:"expected_size,omitempty"`
	BytesRead    int64  `json:"bytes_read"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	PackID       string `json:"pack_id,omitempty"`
}

type Report struct {
	Scope              Scope          `json:"scope"`
	StartedAt          time.Time      `json:"started_at"`
	FinishedAt         time.Time      `json:"finished_at"`
	State              string         `json:"state"`
	RowsSelected       int            `json:"rows_selected"`
	NextAfterID        int64          `json:"next_after_id"`
	More               bool           `json:"more"`
	ReferencesSelected int            `json:"references_selected"`
	DistinctContents   int            `json:"distinct_contents"`
	ContentsOpened     int            `json:"contents_opened"`
	ContentsVerified   int            `json:"contents_verified"`
	ReferencesVerified int            `json:"references_verified"`
	ReferencesAffected int            `json:"references_affected"`
	BytesRead          int64          `json:"bytes_read"`
	Counts             map[string]int `json:"counts"`
	StopReason         string         `json:"stop_reason,omitempty"`
	References         []Reference    `json:"references"`
}

type content struct {
	hash  string
	paths []string
	refs  []int
}

// Run selects once, releases SQL resources, then consumes and closes each
// distinct content stream. The caller must supply the daemon's shared store
// so pack retirement uses its existing reader leases. A report is not a global
// SQL/filesystem snapshot. Concurrent authority changes are inconclusive.
func Run(ctx context.Context, st *store.Store, blobs StreamStore, root string, opts Options) (Report, error) {
	report := Report{Scope: Scope{AfterID: opts.AfterID, Limit: opts.Limit, MaxBytes: opts.MaxBytes, Timeout: opts.Timeout.String()}, StartedAt: time.Now().UTC(), State: "inconclusive", NextAfterID: opts.AfterID,
		Counts: map[string]int{}, References: []Reference{}}
	if err := opts.Validate(); err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	rows, more, err := st.SelectAttachmentVerificationRows(ctx, opts.AfterID, opts.Limit)
	if err != nil {
		return report, err
	}
	report.RowsSelected, report.More = len(rows), more
	var contents []*content
	byHash := map[string]*content{}
	add := func(row store.AttachmentVerificationRow, kind, hash, path string, size *int64) {
		ref := Reference{AttachmentID: row.ID, MessageID: row.MessageID, Kind: kind,
			Hash: strings.ToLower(hash), ExpectedSize: size, Status: NotChecked}
		switch {
		case len(path) > 4096:
			ref.Status, ref.Reason = Inconclusive, "recorded path exceeds 4096 bytes"
		case hash == "":
			ref.Status, ref.Reason = NotMaterialized, "no retained content hash; bytes not verified"
			// Only explicit policy skips with no local locator establish intentional omission.
			if kind == "content" && (path == "" || remotePath(path)) && row.State == string(attachmentpolicy.StateSkipped) && policySkip(row.SkipReason) {
				ref.Status, ref.Reason = Omitted, "recorded policy omission"
			}
			if path != "" && !remotePath(path) {
				ref.Status, ref.Reason = Inconclusive, "local path has no expected content hash"
			}
		case export.ValidateContentHash(hash) != nil:
			ref.Status, ref.Reason = Inconclusive, "invalid expected SHA-256"
		case remotePath(path):
			ref.Status, ref.Reason = NotMaterialized, "external locator; no local storage claim"
		}
		index := len(report.References)
		report.References = append(report.References, ref)
		if ref.Status != NotChecked {
			return
		}
		item := byHash[ref.Hash]
		if item == nil {
			item = &content{hash: ref.Hash}
			byHash[ref.Hash] = item
			contents = append(contents, item)
		}
		item.refs = append(item.refs, index)
		if path != "" {
			item.paths = append(item.paths, path)
		}
	}
	for _, row := range rows {
		var size *int64
		if row.Size.Valid {
			value := row.Size.Int64
			size = &value
		}
		add(row, "content", row.ContentHash, row.StoragePath, size)
		if row.ThumbnailHash != "" || row.ThumbnailPath != "" {
			add(row, "thumbnail", row.ThumbnailHash, row.ThumbnailPath, nil)
		}
	}
	report.ReferencesSelected, report.DistinctContents = len(report.References), len(contents)
	for _, item := range contents {
		if ctx.Err() != nil {
			report.StopReason = "cancelled or timeout"
			break
		}
		if report.BytesRead >= opts.MaxBytes {
			report.StopReason = "byte limit"
			break
		}
		status, reason, packID, n, stopped, checked := check(ctx, st, blobs, root, item, opts.MaxBytes-report.BytesRead)
		report.BytesRead += n
		if checked {
			report.ContentsOpened++
		}
		if status == Verified {
			report.ContentsVerified++
		}
		for _, index := range item.refs {
			ref := &report.References[index]
			ref.Status, ref.Reason, ref.PackID, ref.BytesRead = status, reason, packID, n
			if status == Verified && ref.ExpectedSize != nil && *ref.ExpectedSize != n {
				ref.Status, ref.Reason = MetadataMismatch, "verified content length differs from recorded occurrence size"
			}
		}
		if stopped != "" {
			report.StopReason = stopped
			break
		}
	}
	report.State = "complete"
	for _, ref := range report.References {
		report.Counts[ref.Status]++
		switch ref.Status {
		case Verified:
			report.ReferencesVerified++
		case MissingLoose, PackUnavailable, Corrupt, MetadataMismatch:
			report.ReferencesAffected++
		case Inconclusive, NotMaterialized, NotChecked:
			report.State = "inconclusive"
		}
	}
	if report.More || report.StopReason != "" {
		report.State = "partial"
	}
	unfinished := make(map[int64]bool)
	for _, ref := range report.References {
		if ref.Status == NotChecked {
			unfinished[ref.AttachmentID] = true
		}
	}
	for _, row := range rows {
		if unfinished[row.ID] {
			break
		}
		report.NextAfterID = row.ID
	}
	report.FinishedAt = time.Now().UTC()
	return report, nil
}

func remotePath(path string) bool {
	path = strings.ToLower(path)
	return strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://")
}

func policySkip(reason string) bool {
	switch attachmentpolicy.SkipReason(reason) {
	case attachmentpolicy.SkipPolicyScope, attachmentpolicy.SkipParticipantThreshold, attachmentpolicy.SkipAccountPolicy, attachmentpolicy.SkipSizeCap:
		return true
	default:
		return false
	}
}

func sameAuthority(a, b store.AttachmentBlobLocation) bool {
	if a.Referenced != b.Referenced || (a.Pack == nil) != (b.Pack == nil) {
		return false
	}
	return a.Pack == nil || *a.Pack == *b.Pack
}

func check(ctx context.Context, st *store.Store, blobs StreamStore, root string, item *content, remaining int64) (status, reason, packID string, n int64, stopped string, checked bool) {
	before, err := st.ResolveAttachmentBlobContext(ctx, item.hash)
	if err != nil {
		return Inconclusive, "catalogue could not be resolved", "", 0, "", false
	}
	if !before.Referenced {
		return Inconclusive, "reference is no longer live", "", 0, "", false
	}
	if before.Pack != nil {
		packID = before.Pack.PackID
	}
	reader, _, err := blobs.OpenStream(ctx, item.hash)
	if errors.Is(err, os.ErrNotExist) && before.Pack == nil {
		// Recheck authority before trying a legacy path. Never use loose fallback
		// to hide a missing authoritative pack. The Kit reader handles migrations
		// of canonical loose files and packs itself.
		current, resolveErr := st.ResolveAttachmentBlobContext(ctx, item.hash)
		if resolveErr != nil || !sameAuthority(before, current) {
			return Inconclusive, "authority changed or became unavailable", packID, 0, "", false
		}
		var source io.ReadCloser
		source, _, _, _, err = attachmentstore.OpenLooseCandidates(root, item.hash, func() ([]string, error) { return item.paths, nil })
		if err == nil {
			reader, err = contentverify.NewReadCloser(source, item.hash)
			if err != nil {
				err = errors.Join(err, source.Close())
			}
		}
	}
	checked = err == nil
	if err == nil {
		// One extra byte distinguishes budget exhaustion from verified EOF. Close
		// is checked even on cancellation, read failure and an incomplete stream.
		n, err = io.CopyBuffer(io.Discard, io.LimitReader(contextReader{ctx, reader}, remaining+1), make([]byte, 64<<10))
		err = errors.Join(err, reader.Close())
		if n > remaining {
			return NotChecked, "stream did not reach verified EOF within byte limit", packID, n, "byte limit", checked
		}
	}
	if ctx.Err() != nil {
		return NotChecked, "cancelled or timeout before verification completed", packID, n, "cancelled or timeout", checked
	}
	after, resolveErr := st.ResolveAttachmentBlobContext(ctx, item.hash)
	if resolveErr != nil || !sameAuthority(before, after) {
		return Inconclusive, "authority changed or became unavailable", packID, n, "", checked
	}
	if err == nil {
		return Verified, "", packID, n, "", checked
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		if before.Pack != nil {
			return PackUnavailable, "authoritative pack could not be opened", packID, n, "", checked
		}
		return MissingLoose, "canonical and selected recorded loose paths are missing", "", n, "", checked
	case errors.Is(packstore.ClassifyIntegrityError(err), packstore.ErrPhysicalCorrupt), errors.Is(err, contentverify.ErrMismatch):
		return Corrupt, "physical reader rejected content or container integrity", packID, n, "", checked
	default:
		// Permissions, generic I/O, limits and close failures do not prove corruption.
		return Inconclusive, boundedError(err), packID, n, "", checked
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func boundedError(err error) string {
	message := fmt.Sprintf("read or close could not establish integrity: %v", err)
	if len(message) > 512 {
		return message[:512]
	}
	return message
}
