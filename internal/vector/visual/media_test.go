package visual

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type memoryOpener struct {
	data     []byte
	openErr  error
	closeErr error
}

func (o memoryOpener) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	if o.openErr != nil {
		return nil, 0, o.openErr
	}
	return &memoryReadCloser{Reader: bytes.NewReader(o.data), closeErr: o.closeErr}, int64(len(o.data)), nil
}

type memoryReadCloser struct {
	*bytes.Reader

	closeErr error
}

func (r *memoryReadCloser) Close() error { return r.closeErr }

func TestInspectMediaAcceptsSniffedStillFormatsAndIgnoresDeclaredType(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		wantMIME string
		width    int64
		height   int64
	}{
		{name: "jpeg", data: encodedJPEG(t, 3, 2), wantMIME: "image/jpeg", width: 3, height: 2},
		{name: "png", data: encodedPNG(t, 4, 3), wantMIME: "image/png", width: 4, height: 3},
		{name: "webp", data: syntheticWebP(5, 4), wantMIME: "image/webp", width: 5, height: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			got, err := InspectMedia(t.Context(), memoryOpener{data: tt.data}, eligibleOccurrence("text/plain"), DefaultMediaPolicy())
			require.NoError(err)
			require.True(got.Eligible)
			require.NotNil(got.Media)
			assert.Equal(tt.wantMIME, got.Media.MIMEType)
			assert.Equal(tt.width, got.Media.Width)
			assert.Equal(tt.height, got.Media.Height)
		})
	}
}

func TestInspectMediaAcceptsBoundedMP4Metadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	got, err := InspectMedia(t.Context(), memoryOpener{data: syntheticMP4(640, 360, 1250)}, eligibleOccurrence("video/quicktime"), DefaultMediaPolicy())
	require.NoError(err)
	require.True(got.Eligible)
	require.NotNil(got.Media)
	assert.Equal("video/mp4", got.Media.MIMEType)
	assert.Equal(int64(640), got.Media.Width)
	assert.Equal(int64(360), got.Media.Height)
	assert.Equal(int64(1250), got.Media.DurationMS)
}

func TestInspectMediaAnimatedGIFRequiresCapability(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	data := encodedAnimatedGIF(t)
	policy := DefaultMediaPolicy()
	got, err := InspectMedia(t.Context(), memoryOpener{data: data}, eligibleOccurrence("image/gif"), policy)
	require.NoError(err)
	assert.False(got.Eligible)
	assert.Equal(ReasonProviderFormatUnsupported, got.Reason)

	policy.AllowAnimatedGIF = true
	got, err = InspectMedia(t.Context(), memoryOpener{data: data}, eligibleOccurrence("image/gif"), policy)
	require.NoError(err)
	require.True(got.Eligible)
	assert.True(got.Media.Animated)
}

func TestInspectMediaStableExclusions(t *testing.T) {
	pngData := encodedPNG(t, 2, 2)
	tests := []struct {
		name       string
		occurrence Occurrence
		opener     memoryOpener
		policy     MediaPolicy
		want       EligibilityReason
	}{
		{name: "unknown role", occurrence: occurrenceWithRole(store.AttachmentRoleUnknown), opener: memoryOpener{data: pngData}, policy: DefaultMediaPolicy(), want: ReasonRoleUnknown},
		{name: "inline role", occurrence: occurrenceWithRole(store.AttachmentRoleInline), opener: memoryOpener{data: pngData}, policy: DefaultMediaPolicy(), want: ReasonRoleIneligible},
		{name: "unavailable", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{openErr: ErrContentUnavailable}, policy: DefaultMediaPolicy(), want: ReasonContentUnavailable},
		{name: "pdf", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: []byte("%PDF-1.7")}, policy: DefaultMediaPolicy(), want: ReasonUnsupportedMediaType},
		{name: "audio", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: []byte("OggS\x00synthetic")}, policy: DefaultMediaPolicy(), want: ReasonUnsupportedMediaType},
		{name: "empty", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{}, policy: DefaultMediaPolicy(), want: ReasonEmptyOrExcludedAsset},
		{name: "malformed png", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: []byte("\x89PNG\r\n\x1a\nshort")}, policy: DefaultMediaPolicy(), want: ReasonMalformedMedia},
		{name: "oversized", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: pngData}, policy: MediaPolicy{MaxBytes: 2, MaxPixels: 100, IncludeImages: true, IncludeVideo: true}, want: ReasonProviderLimit},
		{name: "pixel limit", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: pngData}, policy: MediaPolicy{MaxBytes: 1024, MaxPixels: 3, IncludeImages: true, IncludeVideo: true}, want: ReasonPixelLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InspectMedia(t.Context(), tt.opener, tt.occurrence, tt.policy)
			require.NoError(t, err)
			assert.False(t, got.Eligible)
			assert.Equal(t, tt.want, got.Reason)
		})
	}
}

func TestInspectMediaObservesVerifiedCloseFailure(t *testing.T) {
	closeErr := errors.New("verification failed")
	_, err := InspectMedia(t.Context(), memoryOpener{data: encodedPNG(t, 1, 1), closeErr: closeErr}, eligibleOccurrence("image/png"), DefaultMediaPolicy())
	require.ErrorIs(t, err, closeErr)
}

func TestInspectMediaAcceptsResolvedHashlessCASAlias(t *testing.T) {
	occurrence := eligibleOccurrence("image/png")
	occurrence.BlobHash = testHash
	got, err := InspectMedia(t.Context(), memoryOpener{data: encodedPNG(t, 1, 1)}, occurrence, DefaultMediaPolicy())
	require.NoError(t, err)
	assert.True(t, got.Eligible)
	assert.Equal(t, testHash, got.Media.BlobHash)
}

const testHash = "abababababababababababababababababababababababababababababababab"

func eligibleOccurrence(declaredMIME string) Occurrence {
	return Occurrence{MessageID: 7, BlobHash: testHash, DeclaredMIME: declaredMIME, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit}
}

func occurrenceWithRole(role store.AttachmentRole) Occurrence {
	occurrence := eligibleOccurrence("image/png")
	occurrence.Role = role
	return occurrence
}

func encodedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height))))
	return out.Bytes()
}

func encodedJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height)), nil))
	return out.Bytes()
}

func encodedAnimatedGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	var out bytes.Buffer
	require.NoError(t, gif.EncodeAll(&out, &gif.GIF{Image: []*image.Paletted{
		image.NewPaletted(image.Rect(0, 0, 2, 2), palette),
		image.NewPaletted(image.Rect(0, 0, 2, 2), palette),
	}, Delay: []int{1, 1}}))
	return out.Bytes()
}

func syntheticWebP(width, height int) []byte {
	data := make([]byte, 30)
	copy(data[0:4], "RIFF")
	data[4] = 22
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	data[16] = 10
	w := width - 1
	h := height - 1
	data[24], data[25], data[26] = byte(w), byte(w>>8), byte(w>>16)
	data[27], data[28], data[29] = byte(h), byte(h>>8), byte(h>>16)
	return data
}

func syntheticMP4(width, height int, durationMS int64) []byte {
	ftypPayload := append([]byte("isom"), make([]byte, 12)...)
	mvhd := make([]byte, 20)
	putUint32(mvhd[12:16], 1000)
	putUint32(mvhd[16:20], uint32(durationMS))
	tkhd := make([]byte, 84)
	putUint32(tkhd[len(tkhd)-8:len(tkhd)-4], uint32(width<<16))
	putUint32(tkhd[len(tkhd)-4:], uint32(height<<16))
	trak := mp4Box("trak", mp4Box("tkhd", tkhd))
	moov := mp4Box("moov", append(mp4Box("mvhd", mvhd), trak...))
	return append(mp4Box("ftyp", ftypPayload), moov...)
}

func mp4Box(kind string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	putUint32(box[:4], uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], payload)
	return box
}

func putUint32(dst []byte, value uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(value>>24), byte(value>>16), byte(value>>8), byte(value)
}
