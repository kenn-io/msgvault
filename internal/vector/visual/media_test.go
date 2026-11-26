package visual

import (
	"fmt"
	"io/fs"
	"slices"

	"bytes"
	"context"
	"errors"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/voyage"
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

// fullyAuthorizedPolicy is the default policy with every document capability
// authorized, standing in for a complete probed manifest.
func fullyAuthorizedPolicy() MediaPolicy {
	policy := DefaultMediaPolicy()
	policy.AllowAnimatedGIF = true
	for _, capability := range voyage.Capabilities() {
		if capability.Kind == voyage.CapabilityKindDocument {
			policy.AuthorizedCapabilities = append(policy.AuthorizedCapabilities, capability.ID)
		}
	}
	slices.Sort(policy.AuthorizedCapabilities)
	return policy
}

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
		{name: "webp", data: mediatest.WebP(5, 4), wantMIME: "image/webp", width: 5, height: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			got, err := InspectMedia(t.Context(), memoryOpener{data: tt.data}, eligibleOccurrence("text/plain"), fullyAuthorizedPolicy())
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
	got, err := InspectMedia(t.Context(), memoryOpener{data: mediatest.MP4(640, 360, 1250)}, eligibleOccurrence("video/quicktime"), fullyAuthorizedPolicy())
	require.NoError(err)
	require.True(got.Eligible)
	require.NotNil(got.Media)
	assert.Equal("video/mp4", got.Media.MIMEType)
	assert.Equal(int64(640), got.Media.Width)
	// Detection reports the coded height (360 padded to the 16-pixel
	// macroblock grid), the honest bound for provider input.
	assert.Equal(int64(368), got.Media.Height)
	assert.Equal(int64(1250), got.Media.DurationMS)
}

func TestInspectMediaAnimatedGIFRequiresCapability(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	data := encodedAnimatedGIF(t)
	policy := fullyAuthorizedPolicy()
	policy.AllowAnimatedGIF = false
	got, err := InspectMedia(t.Context(), memoryOpener{data: data}, eligibleOccurrence("image/gif"), policy)
	require.NoError(err)
	assert.False(got.Eligible)
	assert.Equal(ReasonProviderFormatUnsupported, got.Reason)

	policy.AllowAnimatedGIF = true
	got, err = InspectMedia(t.Context(), memoryOpener{data: data}, eligibleOccurrence("image/gif"), policy)
	require.NoError(err)
	require.True(got.Eligible)
	assert.True(got.Media.Animated)

	// Config permission without probed authority still fails closed.
	policy.AuthorizedCapabilities = nil
	got, err = InspectMedia(t.Context(), memoryOpener{data: data}, eligibleOccurrence("image/gif"), policy)
	require.NoError(err)
	assert.False(got.Eligible)
	assert.Equal(ReasonProviderFormatUnsupported, got.Reason)
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
		{name: "blob missing from store", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{openErr: fmt.Errorf("open blob: %w", fs.ErrNotExist)}, policy: DefaultMediaPolicy(), want: ReasonContentUnavailable},
		{name: "pdf", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: []byte("%PDF-1.7")}, policy: DefaultMediaPolicy(), want: ReasonUnsupportedMediaType},
		{name: "audio", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: []byte("OggS\x00synthetic")}, policy: DefaultMediaPolicy(), want: ReasonUnsupportedMediaType},
		{name: "empty", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{}, policy: DefaultMediaPolicy(), want: ReasonEmptyOrExcludedAsset},
		{name: "malformed png", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: []byte("\x89PNG\r\n\x1a\nshort")}, policy: DefaultMediaPolicy(), want: ReasonMalformedMedia},
		{name: "oversized", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: pngData}, policy: MediaPolicy{MaxBytes: 2, MaxPixels: 100, IncludeImages: true, IncludeVideo: true}, want: ReasonProviderLimit},
		{name: "pixel limit", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: pngData}, policy: MediaPolicy{MaxBytes: 1024, MaxPixels: 3, IncludeImages: true, IncludeVideo: true}, want: ReasonPixelLimit},
		{name: "unprobed format", occurrence: eligibleOccurrence("image/png"), opener: memoryOpener{data: pngData}, policy: DefaultMediaPolicy(), want: ReasonProviderFormatUnsupported},
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
	_, err := InspectMedia(t.Context(), memoryOpener{data: encodedPNG(t, 1, 1), closeErr: closeErr}, eligibleOccurrence("image/png"), fullyAuthorizedPolicy())
	require.ErrorIs(t, err, closeErr)
}

func TestInspectMediaAcceptsResolvedHashlessCASAlias(t *testing.T) {
	occurrence := eligibleOccurrence("image/png")
	occurrence.BlobHash = testHash
	got, err := InspectMedia(t.Context(), memoryOpener{data: encodedPNG(t, 1, 1)}, occurrence, fullyAuthorizedPolicy())
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
