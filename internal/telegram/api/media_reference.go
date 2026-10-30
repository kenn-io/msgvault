package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// MediaReference identifies a downloadable file. Telegram file references can
// expire, so a retry must refresh the containing message before downloading.
type MediaReference struct {
	Location tg.InputFileLocationClass
	DCID     int
	Size     int64
	Kind     string
	Filename string
	MIMEType string
}

type persistedMediaReference struct {
	Kind          string `json:"kind"`
	ID            int64  `json:"id"`
	AccessHash    int64  `json:"access_hash"`
	FileReference []byte `json:"file_reference"`
	DCID          int    `json:"dc_id"`
	Size          int64  `json:"size"`
	ThumbSize     string `json:"thumb_size,omitempty"`
}

func (r *MediaReference) liveMedia() (*telegram.LiveMedia, error) {
	stored := persistedMediaReference{DCID: r.DCID, Size: r.Size}
	switch location := r.Location.(type) {
	case *tg.InputDocumentFileLocation:
		stored.Kind, stored.ID, stored.AccessHash, stored.FileReference = "document", location.ID, location.AccessHash, location.FileReference
	case *tg.InputPhotoFileLocation:
		stored.Kind, stored.ID, stored.AccessHash, stored.FileReference, stored.ThumbSize = "photo", location.ID, location.AccessHash, location.FileReference, location.ThumbSize
	default:
		return nil, errors.New("unsupported Telegram media location")
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode Telegram media reference: %w", err)
	}
	return &telegram.LiveMedia{
		SourceID: fmt.Sprintf("%s:%d", stored.Kind, stored.ID), Filename: r.Filename,
		MIMEType: r.MIMEType, Kind: r.Kind, Metadata: string(data),
	}, nil
}

func mediaReferenceFromStored(media *telegram.LiveMedia) (*MediaReference, error) {
	if media == nil {
		return nil, errors.New("missing Telegram media reference")
	}
	var stored persistedMediaReference
	decoder := json.NewDecoder(strings.NewReader(media.Metadata))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode Telegram media reference: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid trailing Telegram media reference")
	}
	if stored.ID == 0 || stored.DCID <= 0 || stored.Size < 0 || media.SourceID != fmt.Sprintf("%s:%d", stored.Kind, stored.ID) {
		return nil, errors.New("invalid Telegram media reference")
	}
	ref := &MediaReference{DCID: stored.DCID, Size: stored.Size, Kind: media.Kind, Filename: media.Filename, MIMEType: media.MIMEType}
	switch stored.Kind {
	case "document":
		if stored.ThumbSize != "" {
			return nil, errors.New("invalid Telegram document reference")
		}
		ref.Location = &tg.InputDocumentFileLocation{ID: stored.ID, AccessHash: stored.AccessHash, FileReference: stored.FileReference}
	case "photo":
		if stored.ThumbSize == "" {
			return nil, errors.New("invalid Telegram photo reference")
		}
		ref.Location = &tg.InputPhotoFileLocation{ID: stored.ID, AccessHash: stored.AccessHash, FileReference: stored.FileReference, ThumbSize: stored.ThumbSize}
	default:
		return nil, errors.New("unsupported Telegram media reference")
	}
	return ref, nil
}

// MediaReferenceFromMessage maps the downloadable part of Telegram media.
// Media without a downloadable file returns nil; unsupported file variants
// fail closed so a caller cannot checkpoint past a lost attachment.
func MediaReferenceFromMessage(media tg.MessageMediaClass) (*MediaReference, error) {
	switch value := media.(type) {
	case *tg.MessageMediaEmpty, *tg.MessageMediaGeo, *tg.MessageMediaGeoLive,
		*tg.MessageMediaContact, *tg.MessageMediaVenue, *tg.MessageMediaPoll,
		*tg.MessageMediaDice, *tg.MessageMediaWebPage:
		return nil, nil
	case *tg.MessageMediaDocument:
		if value == nil {
			return nil, errors.New("invalid Telegram document media")
		}
		doc, ok := value.Document.(*tg.Document)
		if !ok || doc == nil || doc.ID == 0 || doc.DCID <= 0 || doc.Size < 0 {
			return nil, errors.New("Telegram document reference is unavailable")
		}
		ref := &MediaReference{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: append([]byte(nil), doc.FileReference...)},
			DCID:     doc.DCID, Size: doc.Size, Kind: "file", MIMEType: doc.MimeType,
		}
		for _, attribute := range doc.Attributes {
			switch item := attribute.(type) {
			case *tg.DocumentAttributeFilename:
				if item != nil {
					ref.Filename = item.FileName
				}
			case *tg.DocumentAttributeAudio:
				if item != nil && item.Voice {
					ref.Kind = "voice_message"
				}
			case *tg.DocumentAttributeVideo:
				if item != nil && item.RoundMessage {
					ref.Kind = "video_message"
				}
			case *tg.DocumentAttributeSticker:
				ref.Kind = "sticker"
			}
		}
		return ref, nil
	case *tg.MessageMediaPhoto:
		if value == nil {
			return nil, errors.New("invalid Telegram photo media")
		}
		photo, ok := value.Photo.(*tg.Photo)
		if !ok || photo == nil || photo.ID == 0 || photo.DCID <= 0 {
			return nil, errors.New("Telegram photo reference is unavailable")
		}
		var thumb string
		var area, size int64
		for _, candidate := range photo.Sizes {
			var kind string
			var w, h, bytes int
			switch item := candidate.(type) {
			case *tg.PhotoSize:
				if item != nil {
					kind, w, h, bytes = item.Type, item.W, item.H, item.Size
				}
			case *tg.PhotoSizeProgressive:
				if item != nil && len(item.Sizes) > 0 {
					kind, w, h, bytes = item.Type, item.W, item.H, item.Sizes[len(item.Sizes)-1]
				}
			}
			if kind == "" || w <= 0 || h <= 0 || int64(w) > math.MaxInt32 || int64(h) > math.MaxInt32 || bytes <= 0 {
				continue
			}
			if pixels := int64(w) * int64(h); pixels > area {
				thumb, area, size = kind, pixels, int64(bytes)
			}
		}
		if thumb == "" {
			return nil, errors.New("Telegram photo has no downloadable size")
		}
		return &MediaReference{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: append([]byte(nil), photo.FileReference...), ThumbSize: thumb},
			DCID:     photo.DCID, Size: size, Kind: "photo", Filename: fmt.Sprintf("photo-%d.jpg", photo.ID), MIMEType: "image/jpeg",
		}, nil
	default:
		return nil, errors.New("unsupported Telegram media")
	}
}
