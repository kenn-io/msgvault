package visual

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"math/bits"
	"strings"

	_ "image/jpeg" // Register the JPEG decoder used by image.DecodeConfig.
	_ "image/png"  // Register the PNG decoder used by image.DecodeConfig.

	"go.kenn.io/msgvault/internal/store"
)

const defaultMaxMediaBytes int64 = 20 << 20
const defaultMaxPixels int64 = 16_000_000

var ErrContentUnavailable = errors.New("visual attachment content unavailable")

type StreamOpener interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

type MediaPolicy struct {
	MaxBytes         int64
	MaxPixels        int64
	IncludeImages    bool
	IncludeVideo     bool
	AllowAnimatedGIF bool
}

func DefaultMediaPolicy() MediaPolicy {
	return MediaPolicy{
		MaxBytes:      defaultMaxMediaBytes,
		MaxPixels:     defaultMaxPixels,
		IncludeImages: true,
		IncludeVideo:  true,
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
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = defaultMaxMediaBytes
	}
	if policy.MaxPixels <= 0 {
		policy.MaxPixels = defaultMaxPixels
	}
	reader, size, err := opener.OpenStream(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrContentUnavailable) {
			return Eligibility{Reason: ReasonContentUnavailable}, nil
		}
		return Eligibility{}, err
	}
	if size > policy.MaxBytes {
		_ = reader.Close()
		return Eligibility{Reason: ReasonProviderLimit}, nil
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, policy.MaxBytes+1))
	closeErr := reader.Close()
	if int64(len(data)) > policy.MaxBytes {
		return Eligibility{Reason: ReasonProviderLimit}, nil
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return Eligibility{}, fmt.Errorf("read verified visual attachment: %w", err)
	}
	if len(data) == 0 {
		return Eligibility{Reason: ReasonEmptyOrExcludedAsset}, nil
	}

	media, reason := inspectMediaBytes(data)
	if reason != "" {
		return Eligibility{Reason: reason}, nil
	}
	media.BlobHash = hash
	media.Bytes = data
	if media.Kind == MediaKindImage && !policy.IncludeImages {
		return Eligibility{Reason: ReasonProviderFormatUnsupported}, nil
	}
	if media.Kind == MediaKindVideo && !policy.IncludeVideo {
		return Eligibility{Reason: ReasonProviderFormatUnsupported}, nil
	}
	if media.MIMEType == "image/gif" && !policy.AllowAnimatedGIF {
		return Eligibility{Reason: ReasonProviderFormatUnsupported}, nil
	}
	if media.Width <= 0 || media.Height <= 0 {
		return Eligibility{Reason: ReasonMalformedMedia}, nil
	}
	if media.Width > policy.MaxPixels || media.Height > policy.MaxPixels ||
		media.Width*media.Height > policy.MaxPixels {
		return Eligibility{Reason: ReasonPixelLimit}, nil
	}
	return Eligibility{Eligible: true, Media: media}, nil
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

func inspectMediaBytes(data []byte) (*MediaInput, EligibilityReason) {
	switch {
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return inspectImage(data, "image/jpeg")
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return inspectImage(data, "image/png")
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		width, height, ok := webPDimensions(data)
		if !ok {
			return nil, ReasonMalformedMedia
		}
		return &MediaInput{Kind: MediaKindImage, MIMEType: "image/webp", Width: width, Height: height}, ""
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		width, height, frames, ok := gifMetadata(data)
		if !ok {
			return nil, ReasonMalformedMedia
		}
		return &MediaInput{Kind: MediaKindImage, MIMEType: "image/gif", Width: width, Height: height, Animated: frames > 1}, ""
	case isMP4(data):
		width, height, durationMS, ok := mp4Metadata(data)
		if !ok {
			return nil, ReasonMalformedMedia
		}
		return &MediaInput{Kind: MediaKindVideo, MIMEType: "video/mp4", Width: width, Height: height, DurationMS: durationMS}, ""
	case bytes.HasPrefix(data, []byte("%PDF")), bytes.HasPrefix(data, []byte("OggS")), bytes.HasPrefix(data, []byte("ID3")):
		return nil, ReasonUnsupportedMediaType
	default:
		return nil, ReasonUnsupportedMediaType
	}
}

// gifMetadata walks container blocks without allocating or decoding pixels.
// It returns logical-screen dimensions and the number of image descriptors.
func gifMetadata(data []byte) (int64, int64, int, bool) {
	if len(data) < 13 {
		return 0, 0, 0, false
	}
	width := int64(binary.LittleEndian.Uint16(data[6:8]))
	height := int64(binary.LittleEndian.Uint16(data[8:10]))
	if width <= 0 || height <= 0 {
		return 0, 0, 0, false
	}
	index := 13
	if data[10]&0x80 != 0 {
		index += 3 * (1 << ((data[10] & 0x07) + 1))
	}
	frames := 0
	for index < len(data) {
		switch data[index] {
		case 0x3b:
			return width, height, frames, frames > 0
		case 0x2c:
			frames++
			if index+10 > len(data) {
				return 0, 0, 0, false
			}
			packed := data[index+9]
			index += 10
			if packed&0x80 != 0 {
				index += 3 * (1 << ((packed & 0x07) + 1))
			}
			if index >= len(data) {
				return 0, 0, 0, false
			}
			index++ // LZW minimum code size.
		case 0x21:
			if index+2 > len(data) {
				return 0, 0, 0, false
			}
			index += 2 // Extension introducer and label.
		default:
			return 0, 0, 0, false
		}
		for {
			if index >= len(data) {
				return 0, 0, 0, false
			}
			size := int(data[index])
			index++
			if size == 0 {
				break
			}
			if index+size > len(data) {
				return 0, 0, 0, false
			}
			index += size
		}
	}
	return 0, 0, 0, false
}

func inspectImage(data []byte, mimeType string) (*MediaInput, EligibilityReason) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ReasonMalformedMedia
	}
	return &MediaInput{Kind: MediaKindImage, MIMEType: mimeType, Width: int64(config.Width), Height: int64(config.Height)}, ""
}

func webPDimensions(data []byte) (int64, int64, bool) {
	if len(data) < 21 {
		return 0, 0, false
	}
	switch string(data[12:16]) {
	case "VP8X":
		if len(data) < 30 {
			return 0, 0, false
		}
		width := 1 + int64(data[24]) + int64(data[25])<<8 + int64(data[26])<<16
		height := 1 + int64(data[27]) + int64(data[28])<<8 + int64(data[29])<<16
		return width, height, true
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return int64(bits&0x3fff) + 1, int64((bits>>14)&0x3fff) + 1, true
	case "VP8 ":
		if len(data) < 30 || !bytes.Equal(data[23:26], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, false
		}
		width := binary.LittleEndian.Uint16(data[26:28]) & 0x3fff
		height := binary.LittleEndian.Uint16(data[28:30]) & 0x3fff
		return int64(width), int64(height), width > 0 && height > 0
	default:
		return 0, 0, false
	}
}

func isMP4(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}

type mp4Info struct {
	width, height, durationMS int64
}

func mp4Metadata(data []byte) (int64, int64, int64, bool) {
	var info mp4Info
	if !scanMP4Boxes(data, &info, 0) || info.width <= 0 || info.height <= 0 {
		return 0, 0, 0, false
	}
	return info.width, info.height, info.durationMS, true
}

func scanMP4Boxes(data []byte, info *mp4Info, depth int) bool {
	if depth > 8 {
		return false
	}
	for offset := 0; offset+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if size < 8 || offset+size > len(data) {
			return false
		}
		kind := string(data[offset+4 : offset+8])
		payload := data[offset+8 : offset+size]
		switch kind {
		case "moov", "trak", "mdia", "minf", "stbl":
			if !scanMP4Boxes(payload, info, depth+1) {
				return false
			}
		case "mvhd":
			parseMVHD(payload, info)
		case "tkhd":
			if len(payload) >= 8 {
				info.width = int64(binary.BigEndian.Uint32(payload[len(payload)-8:len(payload)-4]) >> 16)
				info.height = int64(binary.BigEndian.Uint32(payload[len(payload)-4:]) >> 16)
			}
		}
		offset += size
	}
	return true
}

func parseMVHD(payload []byte, info *mp4Info) {
	if len(payload) < 20 {
		return
	}
	var timescale, duration uint64
	if payload[0] == 1 {
		if len(payload) < 32 {
			return
		}
		timescale = uint64(binary.BigEndian.Uint32(payload[20:24]))
		duration = binary.BigEndian.Uint64(payload[24:32])
	} else {
		timescale = uint64(binary.BigEndian.Uint32(payload[12:16]))
		duration = uint64(binary.BigEndian.Uint32(payload[16:20]))
	}
	if timescale > 0 {
		whole := duration / timescale
		if whole > math.MaxInt64/1000 {
			return
		}
		high, low := bits.Mul64(duration%timescale, 1000)
		fractional, _ := bits.Div64(high, low, timescale)
		milliseconds := whole*1000 + fractional
		if milliseconds <= math.MaxInt64 {
			info.durationMS = int64(milliseconds)
		}
	}
}
