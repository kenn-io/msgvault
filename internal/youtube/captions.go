package youtube

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const DefaultCaptionUnits = 2000

var ErrInvalidCaption = errors.New("invalid YouTube caption content")

// CaptionTrack contains the official track identity and version evidence.
type CaptionTrack struct {
	ID      string         `json:"id"`
	ETag    string         `json:"etag"`
	Snippet CaptionSnippet `json:"snippet"`
}

type CaptionSnippet struct {
	TrackKind   string `json:"trackKind"`
	Language    string `json:"language"`
	Status      string `json:"status"`
	LastUpdated string `json:"lastUpdated"`
}

// CaptionAPI is the official, credential-aware list/download boundary.
type CaptionAPI interface {
	ListCaptionTracks(ctx context.Context, videoID string) ([]CaptionTrack, error)
	DownloadCaption(ctx context.Context, trackID string) ([]byte, error)
}

type CaptionOptions struct {
	Enabled        bool
	SourceID       int64
	AttachmentsDir string
	MaxUnits       int
}

type CaptionSummary struct {
	Units, Stored, Unavailable, Errors int
	Partial                            bool
}

type captionPending struct {
	Root  store.YouTubeCaptionRoot `json:"root"`
	Track CaptionTrack             `json:"track"`
}

type captionCursor struct {
	After   int64           `json:"after"`
	Upper   int64           `json:"upper"`
	Pending *captionPending `json:"pending,omitempty"`
}

type captionObservation struct {
	TrackID    string `json:"track_id,omitempty"`
	Version    string `json:"version,omitempty"`
	Status     string `json:"status"`
	ObservedAt string `json:"observed_at"`
}

// SyncCaptions services a finite, independent cycle over persisted video roots.
// A budget cut keeps a pending download without paying to list it again.
// Missing and ASR tracks advance the cursor and are revisited next cycle.
// The caller serializes this with other sync work on the same source.
func SyncCaptions(ctx context.Context, api CaptionAPI, st *store.Store, opts CaptionOptions) (sum CaptionSummary, err error) {
	var deferredErrors []error
	defer func() { err = errors.Join(append(deferredErrors, err)...) }()
	if !opts.Enabled {
		return sum, nil
	}
	if opts.MaxUnits < 0 || opts.AttachmentsDir == "" {
		return sum, errors.New("caption budget must be nonnegative and attachment directory must be set")
	}
	raw, err := st.YouTubeCaptionStateContext(ctx, opts.SourceID)
	if err != nil {
		return sum, err
	}
	var cursor captionCursor
	if raw != "" && json.Unmarshal([]byte(raw), &cursor) != nil {
		return sum, errors.New("invalid YouTube caption cursor")
	}
	if cursor.After < 0 || cursor.Upper < cursor.After || (cursor.Pending != nil && (cursor.Pending.Root.MessageID <= cursor.After || cursor.Pending.Root.MessageID > cursor.Upper)) {
		return sum, errors.New("invalid YouTube caption cursor bounds")
	}
	save := func() error {
		encoded, encodeErr := json.Marshal(cursor)
		if encodeErr != nil {
			return encodeErr
		}
		next := string(encoded)
		if err := st.CompareAndSwapYouTubeCaptionStateContext(ctx, opts.SourceID, raw, next); err != nil {
			return err
		}
		raw = next
		return nil
	}
	if cursor.Upper == 0 {
		cursor.Upper, err = st.YouTubeCaptionRootBoundContext(ctx, opts.SourceID)
		if err != nil {
			return sum, err
		}
		if err := save(); err != nil {
			return sum, err
		}
	}
	for {
		root, err := st.NextYouTubeCaptionRootContext(ctx, opts.SourceID, cursor.After, cursor.Upper)
		if err != nil {
			return sum, err
		}
		if root == nil {
			cursor = captionCursor{} // Next invocation starts a new finite cycle.
			return sum, save()
		}
		if cursor.Pending != nil && cursor.Pending.Root != *root {
			// A root removed between runs must not stall all remaining videos.
			cursor.Pending = nil
			if err := save(); err != nil {
				return sum, err
			}
		}
		if cursor.Pending == nil {
			if opts.MaxUnits-sum.Units < 50 {
				sum.Partial = true
				return sum, nil
			}
			sum.Units += 50
			tracks, err := api.ListCaptionTracks(ctx, root.VideoID)
			if err != nil && !captionUnavailable(err) {
				return sum, err
			}
			status := "missing"
			if err != nil {
				status = "unavailable"
			}
			slices.SortFunc(tracks, func(a, b CaptionTrack) int {
				if order := strings.Compare(a.Snippet.Language, b.Snippet.Language); order != 0 {
					return order
				}
				return strings.Compare(a.ID, b.ID)
			})
			var selected *CaptionTrack
			for _, track := range tracks {
				if strings.EqualFold(track.Snippet.TrackKind, "ASR") {
					status = "asr"
					continue
				}
				if !validID(track.ID) || track.Snippet.Status != "serving" {
					status = "unavailable"
					continue
				}
				selected = &track
				break
			}
			if selected == nil {
				if err := recordCaptionObservation(ctx, st, root.MessageID, captionObservation{Status: status}); err != nil {
					return sum, err
				}
				sum.Unavailable++
				cursor.After = root.MessageID
				if err := save(); err != nil {
					return sum, err
				}
				continue
			}
			unchanged, err := captionUnchanged(ctx, st, root.MessageID, *selected)
			if err != nil {
				return sum, err
			}
			if unchanged {
				if err := st.ClearYouTubeCaptionObservationContext(ctx, root.MessageID); err != nil {
					return sum, err
				}
				cursor.After = root.MessageID
				if err := save(); err != nil {
					return sum, err
				}
				continue
			}
			cursor.Pending = &captionPending{Root: *root, Track: *selected}
			if err := save(); err != nil { // Keep selected version before spending download quota.
				return sum, err
			}
		}
		if opts.MaxUnits-sum.Units < 200 {
			sum.Partial = true
			return sum, nil
		}
		sum.Units += 200
		track := cursor.Pending.Track
		vtt, err := api.DownloadCaption(ctx, track.ID)
		if err != nil {
			if !captionUnavailable(err) && !errors.Is(err, ErrInvalidCaption) {
				return sum, err
			}
			status := "unavailable"
			if errors.Is(err, ErrInvalidCaption) {
				status = "invalid_content"
				deferredErrors = append(deferredErrors, ErrInvalidCaption)
				sum.Errors++
			} else {
				sum.Unavailable++
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.Reason == "couldNotConvert" {
					status = "conversion_failed"
				}
			}
			if err := recordCaptionObservation(ctx, st, root.MessageID, captionObservation{TrackID: track.ID, Status: status}); err != nil {
				return sum, err
			}
		} else {
			storeErr := storeCaption(ctx, st, opts.AttachmentsDir, root.MessageID, track, vtt)
			switch {
			case storeErr == nil:
				sum.Stored++
			case errors.Is(storeErr, ErrInvalidCaption):
				if err := recordCaptionObservation(ctx, st, root.MessageID, captionObservation{TrackID: track.ID, Status: "invalid_content"}); err != nil {
					return sum, err
				}
				deferredErrors = append(deferredErrors, storeErr)
				sum.Errors++
			default:
				return sum, storeErr
			}
		}
		cursor.After, cursor.Pending = root.MessageID, nil
		if err := save(); err != nil {
			return sum, err
		}
	}
}

func captionUnavailable(err error) bool {
	var api *APIError
	return errors.As(err, &api) && (api.StatusCode == http.StatusNotFound ||
		(api.StatusCode == http.StatusForbidden && api.Reason == "forbidden") ||
		(api.StatusCode == http.StatusBadRequest && api.Reason == "couldNotConvert"))
}

func captionVersion(track CaptionTrack) string {
	// A length prefix keeps the two opaque version fields unambiguous.
	return strconv.Itoa(len(track.ETag)) + ":" + track.ETag + track.Snippet.LastUpdated
}

func captionPartKey(trackID string) string { return "youtube:caption:" + trackID + ":text" }

func captionUnchanged(ctx context.Context, st *store.Store, rootID int64, track CaptionTrack) (bool, error) {
	refs, err := st.MessageYouTubeCaptionAttachmentsContext(ctx, rootID)
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		if ref.SourcePartKey != captionPartKey(track.ID) || ref.State != attachmentpolicy.StateStored {
			continue
		}
		var observation captionObservation
		if json.Unmarshal([]byte(ref.Metadata), &observation) == nil && observation.Version == captionVersion(track) && (track.ETag != "" || track.Snippet.LastUpdated != "") {
			return true, nil
		}
	}
	return false, nil
}

func recordCaptionObservation(ctx context.Context, st *store.Store, rootID int64, observation captionObservation) error {
	observation.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	metadata, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	return st.UpsertAttachmentRecordWithStats(ctx, rootID, store.AttachmentWrite{
		Filename: "youtube-captions.txt", MIMEType: "text/plain", SourcePartKey: "youtube:captions:status",
		SourceAttachmentID: "youtube:caption:status",
		Metadata:           string(metadata), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		State: attachmentpolicy.StateUnavailable, SkipReason: attachmentpolicy.SkipSourceUnavailable,
	}, true)
}

func storeCaption(ctx context.Context, st *store.Store, dir string, rootID int64, track CaptionTrack, vtt []byte) error {
	if len(vtt) > maxAPIBytes {
		return ErrInvalidCaption
	}
	text, err := TranscriptText(vtt)
	if err != nil {
		return errors.Join(ErrInvalidCaption, err)
	}
	path, hash, size, err := export.StoreAttachmentStream(ctx, dir, bytes.NewBufferString(text), maxAPIBytes)
	if err != nil {
		return fmt.Errorf("store YouTube caption: %w", err)
	}
	metadata, err := json.Marshal(captionObservation{TrackID: track.ID, Version: captionVersion(track), Status: "stored", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	if err := st.UpsertAttachmentRecordWithStats(ctx, rootID, store.AttachmentWrite{
		Filename: "youtube-transcript.txt", MIMEType: "text/plain", StoragePath: path, ContentHash: hash, Size: size,
		SourceAttachmentID: "youtube:caption:track:" + track.ID, SourcePartKey: captionPartKey(track.ID), Metadata: string(metadata),
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, State: attachmentpolicy.StateStored,
	}, true); err != nil {
		return err
	}
	return st.ClearYouTubeCaptionObservationContext(ctx, rootID)
}
