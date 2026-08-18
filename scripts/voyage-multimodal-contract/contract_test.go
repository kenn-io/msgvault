//go:build voyage_contract

package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector/visual"
)

const (
	contractDimension        = 1024
	animatedGIFMotionEnabled = false
)

func TestVoyageMultimodalContract_StillFormats(t *testing.T) {
	client := contractClient(t)
	fixtures := []struct {
		mime string
		data []byte
	}{
		{mime: "image/jpeg", data: encodeJPEG(t, patternedImage(color.RGBA{R: 230, A: 255}))},
		{mime: "image/png", data: encodePNG(t, patternedImage(color.RGBA{G: 230, A: 255}))},
		{mime: "image/webp", data: transcodeImage(t, "webp", patternedImage(color.RGBA{B: 230, A: 255}))},
	}
	documents := make([]visual.DocumentInput, len(fixtures))
	for index, fixture := range fixtures {
		documents[index] = imageDocument(index+1, fixture.mime, fixture.data, "a geometric color card")
	}

	results, err := client.EmbedDocuments(t.Context(), documents)
	require.NoError(t, err)
	require.Len(t, results, len(documents))
	for index := range results {
		assert.Equal(t, documents[index].Owner, results[index].Owner)
		assertVector(t, results[index].Vector)
	}
}

func TestVoyageMultimodalContract_MP4(t *testing.T) {
	client := contractClient(t)
	red := encodeMP4(t, "red")
	blue := encodeMP4(t, "blue")
	documents := []visual.DocumentInput{
		videoDocument(11, red, "a short red video"),
		videoDocument(12, blue, "a short blue video"),
	}
	results, err := client.EmbedDocuments(t.Context(), documents)
	require.NoError(t, err)
	require.Len(t, results, 2)
	textQuery, _, err := client.EmbedQuery(t.Context(), visual.QueryInput{Text: "red color video"})
	require.NoError(t, err)
	imageQuery, _, err := client.EmbedQuery(t.Context(), visual.QueryInput{Image: imageMedia(
		"image/png", encodePNG(t, patternedImage(color.RGBA{R: 230, A: 255})),
	)})
	require.NoError(t, err)
	assert.Greater(t, cosine(textQuery, results[0].Vector), cosine(textQuery, results[1].Vector))
	assert.Greater(t, cosine(imageQuery, results[0].Vector), cosine(imageQuery, results[1].Vector))
}

func TestVoyageMultimodalContract_TextAndImageQueries(t *testing.T) {
	client := contractClient(t)
	red := encodePNG(t, patternedImage(color.RGBA{R: 230, A: 255}))
	blue := encodePNG(t, patternedImage(color.RGBA{B: 230, A: 255}))
	documents := []visual.DocumentInput{
		imageDocument(21, "image/png", red, "red geometric color card"),
		imageDocument(22, "image/png", blue, "blue geometric color card"),
	}
	results, err := client.EmbedDocuments(t.Context(), documents)
	require.NoError(t, err)

	textVector, textUsage, err := client.EmbedQuery(t.Context(), visual.QueryInput{Text: "red geometric card"})
	require.NoError(t, err)
	assertVector(t, textVector)
	if textUsage.Available {
		assert.GreaterOrEqual(t, textUsage.TotalTokens, int64(0))
	}
	imageVector, _, err := client.EmbedQuery(t.Context(), visual.QueryInput{
		Text: "matching reference image", Image: imageMedia("image/png", red),
	})
	require.NoError(t, err)
	assertVector(t, imageVector)
	assert.Greater(t, cosine(textVector, results[0].Vector), cosine(textVector, results[1].Vector))
	assert.Greater(t, cosine(imageVector, results[0].Vector), cosine(imageVector, results[1].Vector))
}

func TestVoyageMultimodalContract_InterleavedOrder(t *testing.T) {
	client := contractClient(t)
	red := imageDocument(31, "image/png", encodePNG(t, patternedImage(color.RGBA{R: 230, A: 255})), "red card")
	green := imageDocument(32, "image/png", encodePNG(t, patternedImage(color.RGBA{G: 230, A: 255})), "green card")
	blue := imageDocument(33, "image/png", encodePNG(t, patternedImage(color.RGBA{B: 230, A: 255})), "blue card")
	batch, err := client.EmbedDocuments(t.Context(), []visual.DocumentInput{green, blue, red})
	require.NoError(t, err)
	require.Len(t, batch, 3)
	for index, document := range []visual.DocumentInput{green, blue, red} {
		single, singleErr := client.EmbedDocuments(t.Context(), []visual.DocumentInput{document})
		require.NoError(t, singleErr)
		require.Len(t, single, 1)
		assert.Equal(t, document.Owner, batch[index].Owner)
		assert.Greater(t, cosine(batch[index].Vector, single[0].Vector), 0.999)
	}
}

func TestVoyageMultimodalContract_AnimatedGIFMotion(t *testing.T) {
	client := contractClient(t)
	first := encodeAnimatedGIF(t, color.White)
	second := encodeAnimatedGIF(t, color.RGBA{R: 255, A: 255})
	results, err := client.EmbedDocuments(t.Context(), []visual.DocumentInput{
		imageDocument(41, "image/gif", first, "two-frame animation"),
		imageDocument(42, "image/gif", second, "two-frame animation"),
	})
	if err != nil {
		require.ErrorIs(t, err, visual.ErrProviderRejected)
		assert.False(t, animatedGIFMotionEnabled)
		return
	}
	require.Len(t, results, 2)
	motionSensitive := cosine(results[0].Vector, results[1].Vector) < 0.999
	assert.Equal(t, animatedGIFMotionEnabled, motionSensitive,
		"provider GIF motion capability changed; review and update the checked-in capability")
}

func TestVoyageMultimodalContract_Boundaries(t *testing.T) {
	client := contractClient(t)
	_, _, err := client.EmbedQuery(t.Context(), visual.QueryInput{})
	assert.ErrorContains(t, err, "requires text or image")
	_, _, err = client.EmbedQuery(t.Context(), visual.QueryInput{Image: &visual.MediaInput{
		Kind: "video", MIMEType: "video/mp4", Bytes: []byte("not sent"),
	}})
	assert.ErrorContains(t, err, "must be an image")

	limited, err := visual.NewVoyageClient(visual.VoyageConfig{
		Endpoint: "https://api.voyageai.com/v1", APIKey: os.Getenv("VOYAGE_API_KEY"),
		Model: "voyage-multimodal-3.5", Dimension: contractDimension,
		MaxBatchItems: 1, Timeout: 45 * time.Second,
	})
	require.NoError(t, err)
	document := imageDocument(51, "image/png", encodePNG(t, patternedImage(color.RGBA{A: 255})), "boundary")
	_, err = limited.EmbedDocuments(t.Context(), []visual.DocumentInput{document, document})
	assert.ErrorIs(t, err, visual.ErrProviderBatchTooLarge)
}

func contractClient(t *testing.T) *visual.VoyageClient {
	t.Helper()
	key := os.Getenv("VOYAGE_API_KEY")
	if key == "" {
		t.Skip("VOYAGE_API_KEY is required for the opt-in Voyage contract gate")
	}
	client, err := visual.NewVoyageClient(visual.VoyageConfig{
		Endpoint: "https://api.voyageai.com/v1", APIKey: key,
		Model: "voyage-multimodal-3.5", Dimension: contractDimension,
		Timeout: 45 * time.Second, MaxRetries: 3,
	})
	require.NoError(t, err)
	return client
}

func imageDocument(id int, mime string, data []byte, contextText string) visual.DocumentInput {
	media := imageMedia(mime, data)
	return visual.DocumentInput{
		Owner:    visual.Owner{MessageID: int64(id), BlobHash: media.BlobHash, MediaInputKey: visual.OriginalMediaInputKey},
		Revision: fmt.Sprintf("contract-image-%d", id),
		Parts:    []visual.InputPart{{Text: contextText}, {Media: media}},
	}
}

func videoDocument(id int, data []byte, contextText string) visual.DocumentInput {
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	return visual.DocumentInput{
		Owner:    visual.Owner{MessageID: int64(id), BlobHash: hash, MediaInputKey: visual.OriginalMediaInputKey},
		Revision: fmt.Sprintf("contract-video-%d", id),
		Parts: []visual.InputPart{{Text: contextText}, {Media: &visual.MediaInput{
			Kind: "video", MIMEType: "video/mp4", BlobHash: hash, Bytes: data,
			Width: 64, Height: 64, DurationMS: 1000,
		}}},
	}
}

func imageMedia(mime string, data []byte) *visual.MediaInput {
	return &visual.MediaInput{
		Kind: "image", MIMEType: mime, BlobHash: fmt.Sprintf("%x", sha256.Sum256(data)),
		Bytes: data, Width: 64, Height: 64,
	}
}

func patternedImage(base color.RGBA) image.Image {
	result := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			shade := uint8((x*3 + y*5) % 25)
			pixel := base
			pixel.R = uint8(min(uint16(255), uint16(pixel.R)+uint16(shade)))
			pixel.G = uint8(min(uint16(255), uint16(pixel.G)+uint16(shade)))
			pixel.B = uint8(min(uint16(255), uint16(pixel.B)+uint16(shade)))
			result.SetRGBA(x, y, pixel)
		}
	}
	return result
}

func encodePNG(t *testing.T, value image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, value))
	return output.Bytes()
}

func encodeJPEG(t *testing.T, value image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	require.NoError(t, jpeg.Encode(&output, value, &jpeg.Options{Quality: 90}))
	return output.Bytes()
}

func transcodeImage(t *testing.T, format string, value image.Image) []byte {
	t.Helper()
	input := filepath.Join(t.TempDir(), "input.png")
	output := filepath.Join(t.TempDir(), "output."+format)
	require.NoError(t, os.WriteFile(input, encodePNG(t, value), 0o600))
	command := exec.CommandContext(t.Context(), "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-threads", "1", "-i", input, "-frames:v", "1", "-y", output)
	require.NoError(t, command.Run(), "ffmpeg is required to generate the authenticated contract fixtures")
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	return data
}

func encodeMP4(t *testing.T, colorName string) []byte {
	t.Helper()
	output := filepath.Join(t.TempDir(), "fixture.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-threads", "1", "-f", "lavfi", "-i", "color=c="+colorName+":s=64x64:r=4:d=1",
		"-an", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-y", output)
	require.NoError(t, command.Run(), "ffmpeg with libx264 is required to generate the MP4 fixture")
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	return data
}

func encodeAnimatedGIF(t *testing.T, second color.Color) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White, color.RGBA{R: 255, A: 255}}
	firstFrame := image.NewPaletted(image.Rect(0, 0, 64, 64), palette)
	secondFrame := image.NewPaletted(image.Rect(0, 0, 64, 64), palette)
	for y := range 64 {
		for x := range 64 {
			secondFrame.Set(x, y, second)
		}
	}
	var output bytes.Buffer
	require.NoError(t, gif.EncodeAll(&output, &gif.GIF{
		Image: []*image.Paletted{firstFrame, secondFrame}, Delay: []int{20, 20}, LoopCount: 0,
	}))
	return output.Bytes()
}

func assertVector(t *testing.T, vector []float32) {
	t.Helper()
	require.Len(t, vector, contractDimension)
	for _, value := range vector {
		assert.False(t, math.IsNaN(float64(value)) || math.IsInf(float64(value), 0))
	}
}

func cosine(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return math.NaN()
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		l, r := float64(left[index]), float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return math.NaN()
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}
