package visual

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"

	"go.kenn.io/msgvault/internal/store"
)

const defaultMaxMediaBytes int64 = 20 << 20
const defaultMaxPixels int64 = 16_000_000

var ErrContentUnavailable = errors.New("visual attachment content unavailable")

type StreamOpener interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

// MediaPolicy bounds which attachment occurrences are eligible for hosted
// visual processing. Detection and the byte, pixel, and kind bounds are
// delegated to go.kenn.io/docbank/document/media; on top of them,
// AuthorizedCapabilities restricts eligibility to formats the operator's
// authenticated Voyage capability manifest actually authorizes.
type MediaPolicy struct {
	MaxBytes         int64
	MaxPixels        int64
	IncludeImages    bool
	IncludeVideo     bool
	AllowAnimatedGIF bool
	// AuthorizedCapabilities is the sorted list of docbank capability IDs
	// with probed upload authority. Empty authorizes nothing: eligibility
	// fails closed until an operator supplies a validated manifest.
	AuthorizedCapabilities []string
}

func DefaultMediaPolicy() MediaPolicy {
	return MediaPolicy{
		MaxBytes:      defaultMaxMediaBytes,
		MaxPixels:     defaultMaxPixels,
		IncludeImages: true,
		IncludeVideo:  true,
	}
}

// documentPolicy maps the msgvault policy onto docbank's media bounds.
func (p MediaPolicy) documentPolicy() media.Policy {
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	maxPixels := p.MaxPixels
	if maxPixels <= 0 {
		maxPixels = defaultMaxPixels
	}
	return media.Policy{
		MaxBytes: maxBytes, MaxPixels: maxPixels,
		AllowStill:    p.IncludeImages,
		AllowAnimated: p.IncludeImages && p.AllowAnimatedGIF,
		AllowVideo:    p.IncludeVideo,
	}
}

// authorizes reports whether the manifest-authorized capability set covers
// the detected media as document input.
func (p MediaPolicy) authorizes(metadata media.Metadata) bool {
	capability, ok := documentCapabilityID(metadata)
	if !ok {
		return false
	}
	return slices.Contains(p.AuthorizedCapabilities, capability)
}

// documentCapabilityID names the docbank document capability that must be
// probed before media of this shape may leave the machine.
func documentCapabilityID(metadata media.Metadata) (string, bool) {
	switch metadata.Format {
	case media.FormatJPEG:
		return voyage.CapabilityImageJPEG, true
	case media.FormatPNG:
		if metadata.Animated {
			return "", false
		}
		return voyage.CapabilityImagePNG, true
	case media.FormatWebP:
		if metadata.Animated {
			return "", false
		}
		return voyage.CapabilityImageWebP, true
	case media.FormatGIF:
		if metadata.Animated {
			return voyage.CapabilityImageGIFAnimated, true
		}
		return voyage.CapabilityImageGIFStill, true
	case media.FormatMP4:
		return voyage.CapabilityVideoMP4, true
	default:
		return "", false
	}
}

func InspectMedia(
	ctx context.Context,
	opener StreamOpener,
	occurrence Occurrence,
	policy MediaPolicy,
) (Eligibility, error) {
	if occurrence.Role == store.AttachmentRoleUnknown || !authoritativeRoleSource(occurrence.RoleSource) {
		return Eligibility{Reason: ReasonRoleUnknown}, nil
	}
	if occurrence.Role != store.AttachmentRoleStandalone {
		return Eligibility{Reason: ReasonRoleIneligible}, nil
	}
	hash := strings.ToLower(occurrence.BlobHash)
	if !validBlobHash(hash) {
		return Eligibility{Reason: ReasonContentUnavailable}, nil
	}
	documentPolicy := policy.documentPolicy()
	reader, size, err := opener.OpenStream(ctx, hash)
	if err != nil {
		// A blob missing from the content store is a per-owner condition to
		// record and retry, never a reason to abort the whole scan.
		if errors.Is(err, ErrContentUnavailable) || errors.Is(err, fs.ErrNotExist) {
			return Eligibility{Reason: ReasonContentUnavailable}, nil
		}
		return Eligibility{}, err
	}
	if size > documentPolicy.MaxBytes {
		_ = reader.Close()
		return Eligibility{Reason: ReasonProviderLimit}, nil
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, documentPolicy.MaxBytes+1))
	closeErr := reader.Close()
	if int64(len(data)) > documentPolicy.MaxBytes {
		return Eligibility{Reason: ReasonProviderLimit}, nil
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return Eligibility{}, fmt.Errorf("read verified visual attachment: %w", err)
	}
	if len(data) == 0 {
		return Eligibility{Reason: ReasonEmptyOrExcludedAsset}, nil
	}

	metadata, reason := media.InspectBytes(data, occurrence.DeclaredMIME, documentPolicy)
	if reason != media.ReasonEligible {
		return Eligibility{Reason: eligibilityReason(reason)}, nil
	}
	if !policy.authorizes(metadata) {
		return Eligibility{Reason: ReasonProviderFormatUnsupported}, nil
	}
	input := mediaInputFrom(metadata)
	input.BlobHash = hash
	input.Bytes = data
	return Eligibility{Eligible: true, Media: input}, nil
}

// eligibilityReason maps docbank detection and policy outcomes onto the
// stable reasons this package persists with rejections.
func eligibilityReason(reason media.Reason) EligibilityReason {
	switch reason {
	case media.ReasonEligible:
		return ""
	case media.ReasonUnsupportedMedia:
		return ReasonUnsupportedMediaType
	case media.ReasonMalformedMedia:
		return ReasonMalformedMedia
	case media.ReasonTooLarge, media.ReasonTooLong:
		return ReasonProviderLimit
	case media.ReasonTooManyPixels:
		return ReasonPixelLimit
	case media.ReasonStillNotAllowed, media.ReasonAnimatedNotAllowed, media.ReasonVideoNotAllowed:
		return ReasonProviderFormatUnsupported
	default:
		return ReasonMalformedMedia
	}
}

func mediaInputFrom(metadata media.Metadata) *MediaInput {
	kind := MediaKindImage
	if metadata.Kind == media.KindVideo {
		kind = MediaKindVideo
	}
	durationMS := int64(0)
	if metadata.DurationKnown {
		durationMS = metadata.DurationMS
	}
	return &MediaInput{
		Kind: kind, Format: metadata.Format, MIMEType: metadata.MediaType,
		Width: metadata.Width, Height: metadata.Height,
		DurationMS: durationMS, Animated: metadata.Animated,
	}
}

func authoritativeRoleSource(source store.AttachmentRoleSource) bool {
	switch source {
	case store.AttachmentRoleSourceMIMEDisposition,
		store.AttachmentRoleSourceProviderExplicit,
		store.AttachmentRoleSourceImporterSemantics,
		store.AttachmentRoleSourceRawMIMERepair:
		return true
	default:
		return false
	}
}

func validBlobHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}
