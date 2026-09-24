package docbankmedia_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/docbankmedia"
)

const testKey = "synthetic-api-key"

// Mirrors of the Docbank e33d77e4 request structs; strict decoding rejects
// any member the server would reject.
type wireOccurrence struct {
	Ref          string        `json:"ref"`
	Revision     string        `json:"revision"`
	Filename     string        `json:"filename"`
	PersonRef    string        `json:"person_ref,omitempty"`
	SpeakerLabel string        `json:"speaker_label,omitempty"`
	Message      wireTimestamp `json:"message"`
}

type wireTimestamp struct {
	Normalized     string `json:"normalized"`
	Raw            string `json:"raw"`
	Precision      string `json:"precision"`
	Timezone       string `json:"timezone"`
	ZoneText       string `json:"zone_text,omitempty"`
	OffsetSeconds  *int   `json:"offset_seconds,omitempty"`
	FractionDigits int    `json:"fraction_digits"`
}

type wireSupplied struct {
	OperationID              string          `json:"operation_id"`
	Filename                 string          `json:"filename"`
	MediaType                string          `json:"media_type"`
	SHA256                   string          `json:"sha256"`
	ByteLength               int64           `json:"byte_length"`
	ExistingContentVersionID string          `json:"existing_content_version_id,omitempty"`
	Occurrence               wireOccurrence  `json:"occurrence"`
	Processing               *wireProcessing `json:"processing,omitempty"`
}

type wireArtifact struct {
	OperationID  string `json:"operation_id"`
	OccurrenceID string `json:"occurrence_id"`
	Kind         string `json:"kind"`
	Origin       string `json:"origin,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Language     string `json:"language,omitempty"`
	Filename     string `json:"filename"`
	MediaType    string `json:"media_type"`
	SHA256       string `json:"sha256"`
	ByteLength   int64  `json:"byte_length"`
}

type wireProcessing struct {
	Profile         string `json:"profile"`
	SuppliedInputID string `json:"supplied_input_id,omitempty"`
}

type wireRetry struct {
	OperationID string          `json:"operation_id"`
	Processing  *wireProcessing `json:"processing,omitempty"`
}

type capturedPart struct {
	keys    []string
	raw     []byte
	content []byte
}

func TestClientMediaWire(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	operationID := uuid.NewString()
	audio := []byte("RIFF-synthetic-wav-bytes")
	transcript := []byte("hello transcript")
	var mu sync.Mutex
	parts := map[string]capturedPart{}
	var retry wireRetry
	var retryKeys []string
	var rejected []string
	reject := func(w http.ResponseWriter, err error) {
		mu.Lock()
		rejected = append(rejected, err.Error())
		mu.Unlock()
		http.Error(w, "validation", http.StatusUnprocessableEntity)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(testKey, r.Header.Get("X-Api-Key")) || !assert.Empty(r.Header.Get("Authorization")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/media/sources":
			var metadata wireSupplied
			part, err := readStrictMultipart(r, &metadata)
			if err != nil {
				reject(w, err)
				return
			}
			mu.Lock()
			parts["submit"] = part
			mu.Unlock()
			writeJSON(w, docbankmedia.Receipt{VaultUID: "vault", SourceID: "source", SourceVersionID: "version",
				ContentVersionID: "content", OccurrenceID: "occurrence", OperationID: metadata.OperationID,
				OperationState: "succeeded", CoverageState: "unprocessed"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/media/sources/source/artifacts":
			var metadata wireArtifact
			part, err := readStrictMultipart(r, &metadata)
			if err != nil {
				reject(w, err)
				return
			}
			mu.Lock()
			parts["artifact"] = part
			mu.Unlock()
			writeJSON(w, docbankmedia.Receipt{VaultUID: "vault", SourceID: "source", OperationID: metadata.OperationID,
				SuppliedInputID: "input", OperationState: "succeeded", CoverageState: "unprocessed"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/media/sources/source/retry":
			assert.Equal("application/json", r.Header.Get("Content-Type"))
			data, err := io.ReadAll(r.Body)
			if err == nil {
				err = json.Unmarshal(data, &retry, json.RejectUnknownMembers(true))
			}
			if err == nil {
				retryKeys, err = jsonKeys(data)
			}
			if err != nil {
				reject(w, err)
				return
			}
			writeJSON(w, docbankmedia.Receipt{VaultUID: "vault", SourceID: "source", OperationID: retry.OperationID,
				JobID: strings.Repeat("d", 64), OperationState: "queued", CoverageState: "pending"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/processing/jobs/"+strings.Repeat("d", 64):
			writeJSON(w, map[string]any{"job_id": strings.Repeat("d", 64), "state": "completed", "phase": "done",
				"embedding_job_ids": []string{}, "completed_bindings": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := docbankmedia.NewClient(server.URL+"/", func() (string, error) { return testKey, nil })
	require.NoError(err)
	offset := 0
	receipt, err := client.Submit(t.Context(), docbankmedia.SuppliedMetadata{
		OperationID: operationID, Filename: "audio.wav", MediaType: "audio/wav",
		SHA256: sha256Hex(audio), ByteLength: int64(len(audio)),
		Occurrence: docbankmedia.Occurrence{Ref: "msgvault:occurrence", Revision: "revision", Filename: "audio.wav",
			Message: docbankmedia.Timestamp{Normalized: "2026-09-16T10:11:12Z", Raw: "2026-09-16T10:11:12Z",
				Precision: "instant", Timezone: "UTC", ZoneText: "Z", OffsetSeconds: &offset}},
	}, bytes.NewReader(audio))
	require.NoError(err)
	assert.Equal("occurrence", receipt.OccurrenceID)
	artifactID := uuid.NewString()
	artifact, err := client.ImportTranscript(t.Context(), "source", docbankmedia.ArtifactMetadata{
		OperationID: artifactID, OccurrenceID: "occurrence", Kind: "transcript", Origin: "provider",
		Provider: "beeper", Language: "en", Filename: "transcript.txt", MediaType: "text/plain",
		SHA256: sha256Hex(transcript), ByteLength: int64(len(transcript)),
	}, bytes.NewReader(transcript))
	require.NoError(err)
	assert.Equal("input", artifact.SuppliedInputID)
	processID := uuid.NewString()
	processed, err := client.Process(t.Context(), "source", processID, artifact.SuppliedInputID)
	require.NoError(err)
	status, err := client.JobStatus(t.Context(), processed.JobID)
	require.NoError(err)
	assert.Equal("completed", status.State)

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(rejected)
	submit := parts["submit"]
	assert.Equal(audio, submit.content)
	assert.Equal([]string{"byte_length", "filename", "media_type", "occurrence", "operation_id", "sha256"}, submit.keys)
	assert.NotContains(string(submit.raw), "transcript")
	parsed, err := uuid.Parse(operationID)
	require.NoError(err)
	assert.Equal(uuid.Version(4), parsed.Version())
	assert.Equal(transcript, parts["artifact"].content)
	assert.Equal([]string{"byte_length", "filename", "kind", "language", "media_type", "occurrence_id",
		"operation_id", "origin", "provider", "sha256"}, parts["artifact"].keys)
	assert.Equal([]string{"operation_id", "processing"}, retryKeys)
	assert.Equal(processID, retry.OperationID)
	assert.Equal(&wireProcessing{Profile: "supplied-transcript", SuppliedInputID: "input"}, retry.Processing)
}

func TestClientMediaTrust(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	credentialURL := (&url.URL{
		Scheme: "https",
		User:   url.UserPassword("user", "pass"),
		Host:   "docbank.example.com",
	}).String()
	for _, endpoint := range []string{
		"http://docbank.example.com", "ftp://127.0.0.1", credentialURL,
		"https://docbank.example.com/path?secret=value", "https://docbank.example.com/path#fragment", "127.0.0.1:8080",
	} {
		_, err := docbankmedia.NewClient(endpoint, nil)
		require.Error(err, endpoint)
	}
	for _, endpoint := range []string{"https://docbank.example.com", "http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		_, err := docbankmedia.NewClient(endpoint, nil)
		require.NoError(err, endpoint)
	}

	var mu sync.Mutex
	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/media/sources/redirect":
			http.Redirect(w, r, "/next", http.StatusFound)
		case "/api/v1/media/sources/huge":
			_, _ = w.Write(bytes.Repeat([]byte(" "), (1<<20)+1))
		default:
			http.Error(w, "secret response body "+r.Header.Get("X-Api-Key"), http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	client, err := docbankmedia.NewClient(server.URL, func() (string, error) { return testKey, nil })
	require.NoError(err)
	_, err = client.Status(t.Context(), "redirect")
	require.Error(err)
	assert.Equal("http_error", docbankmedia.ErrorCode(err))
	_, err = client.Status(t.Context(), "huge")
	require.ErrorIs(err, docbankmedia.ErrResponseTooLarge)
	_, err = client.Status(t.Context(), "unavailable")
	require.Error(err)
	assert.NotContains(err.Error(), "secret")
	assert.NotContains(err.Error(), testKey)
	assert.Equal("server_error", docbankmedia.ErrorCode(err))
	assert.True(docbankmedia.Retryable(err))

	missing, err := docbankmedia.NewClient(server.URL, func() (string, error) {
		return "", errors.New("environment variable holds " + testKey)
	})
	require.NoError(err)
	_, err = missing.Submit(t.Context(), docbankmedia.SuppliedMetadata{OperationID: uuid.NewString(),
		Filename: "a.wav", MediaType: "audio/wav", SHA256: sha256Hex([]byte("x")), ByteLength: 1},
		strings.NewReader("x"))
	require.ErrorIs(err, docbankmedia.ErrCredentialUnavailable)
	assert.NotContains(err.Error(), testKey)
	assert.Equal("credential_unavailable", docbankmedia.ErrorCode(err))
	assert.False(docbankmedia.Retryable(err))

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(hits["/next"], "redirects are not followed")
	assert.Zero(hits["/api/v1/media/sources"], "missing credentials send no bytes")
}

// TestClientMediaProcessReceipts follows Docbank's saved retry receipts: a
// failed enqueue has no job, while a queued receipt must name one.
func TestClientMediaProcessReceipts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OperationID string `json:"operation_id"`
		}
		data, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.NoError(json.Unmarshal(data, &body))
		state, coverage := "failed", "unavailable"
		if strings.Contains(r.URL.Path, "/queued/") {
			state, coverage = "queued", "pending"
		}
		writeJSON(w, docbankmedia.Receipt{VaultUID: "vault", SourceID: "source",
			OperationID: body.OperationID, OperationState: state, CoverageState: coverage})
	}))
	defer server.Close()
	client, err := docbankmedia.NewClient(server.URL, func() (string, error) { return testKey, nil })
	require.NoError(err)

	operationID := uuid.NewString()
	receipt, err := client.Process(t.Context(), "failed", operationID, "input")
	require.NoError(err)
	assert.Equal("failed", receipt.OperationState)
	assert.Equal("unavailable", receipt.CoverageState)
	assert.Empty(receipt.JobID)
	assert.Equal(operationID, receipt.OperationID)

	_, err = client.Process(t.Context(), "queued", uuid.NewString(), "input")
	require.ErrorIs(err, docbankmedia.ErrInvalidReceipt)
}

// TestDocbankMediaLiveContract exercises a real isolated Docbank daemon. It
// skips in ordinary runs and fails when requested without that runtime.
func TestDocbankMediaLiveContract(t *testing.T) {
	endpoint := os.Getenv("MSGVAULT_TEST_DOCBANK_URL")
	if endpoint == "" {
		if !strings.Contains(flag.Lookup("test.run").Value.String(), "TestDocbankMediaLiveContract") {
			t.Skip("MSGVAULT_TEST_DOCBANK_URL selects an isolated Docbank daemon")
		}
		require.FailNow(t, "MSGVAULT_TEST_DOCBANK_URL is required for the live Docbank contract")
	}
	require := require.New(t)
	assert := assert.New(t)
	key := os.Getenv("MSGVAULT_TEST_DOCBANK_API_KEY")
	client, err := docbankmedia.NewClient(endpoint, func() (string, error) { return key, nil })
	require.NoError(err)

	for _, sample := range []struct {
		name, mediaType string
		data            []byte
	}{{"synthetic.wav", "audio/wav", syntheticWAV()}, {"synthetic.mp3", "audio/mpeg", syntheticMP3()}} {
		metadata := docbankmedia.SuppliedMetadata{
			OperationID: uuid.NewString(), Filename: sample.name, MediaType: sample.mediaType,
			SHA256: sha256Hex(sample.data), ByteLength: int64(len(sample.data)),
			Occurrence: docbankmedia.Occurrence{Ref: "msgvault:live-" + sample.name,
				Revision: sha256Hex(sample.data), Filename: sample.name},
		}
		first, err := client.Submit(t.Context(), metadata, bytes.NewReader(sample.data))
		require.NoError(err, sample.name)
		replay, err := client.Submit(t.Context(), metadata, bytes.NewReader(sample.data))
		require.NoError(err, sample.name)
		assert.Equal(first.SourceID, replay.SourceID)
		assert.Equal(first.ContentVersionID, replay.ContentVersionID)
		assert.Equal(first.OccurrenceID, replay.OccurrenceID)

		text := []byte("synthetic provider transcript for " + sample.name)
		artifact, err := client.ImportTranscript(t.Context(), first.SourceID, docbankmedia.ArtifactMetadata{
			OperationID: uuid.NewString(), OccurrenceID: first.OccurrenceID, Kind: "transcript",
			Origin: "provider", Provider: "beeper", Filename: "transcript.txt", MediaType: "text/plain",
			SHA256: sha256Hex(text), ByteLength: int64(len(text)),
		}, bytes.NewReader(text))
		require.NoError(err, sample.name)
		require.NotEmpty(artifact.SuppliedInputID)

		processed, err := client.Process(t.Context(), first.SourceID, uuid.NewString(), artifact.SuppliedInputID)
		if err != nil {
			t.Logf("%s processing refused with %s", sample.name, docbankmedia.ErrorCode(err))
			continue
		}
		job, err := client.JobStatus(t.Context(), processed.JobID)
		require.NoError(err)
		t.Logf("%s job state %s", sample.name, job.State)
	}
}

// readStrictMultipart enforces Docbank's metadata-then-file envelope. It runs
// in the server goroutine, so it returns errors for the handler to report.
func readStrictMultipart(r *http.Request, metadata any) (capturedPart, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return capturedPart{}, fmt.Errorf("read multipart: %w", err)
	}
	first, err := reader.NextPart()
	if err != nil {
		return capturedPart{}, fmt.Errorf("read multipart: %w", err)
	}
	if first.FormName() != "metadata" || first.FileName() != "" {
		return capturedPart{}, errors.New("first multipart part must be metadata")
	}
	raw, err := io.ReadAll(first)
	if err != nil {
		return capturedPart{}, fmt.Errorf("read multipart: %w", err)
	}
	if err := json.Unmarshal(raw, metadata, json.RejectUnknownMembers(true)); err != nil {
		return capturedPart{}, fmt.Errorf("read multipart: %w", err)
	}
	second, err := reader.NextPart()
	if err != nil {
		return capturedPart{}, fmt.Errorf("read multipart: %w", err)
	}
	if second.FormName() != "file" || second.FileName() == "" {
		return capturedPart{}, errors.New("second multipart part must be a file")
	}
	content, err := io.ReadAll(second)
	if err != nil {
		return capturedPart{}, fmt.Errorf("read multipart: %w", err)
	}
	if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
		return capturedPart{}, errors.New("media upload requires exactly metadata and file parts")
	}
	keys, err := jsonKeys(raw)
	return capturedPart{keys: keys, raw: raw, content: content}, err
}

func jsonKeys(raw []byte) ([]string, error) {
	var members map[string]any
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(members)), nil
}

func writeJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// syntheticWAV is one second of 8 kHz mono 16-bit PCM silence.
func syntheticWAV() []byte {
	audio := make([]byte, 16000)
	data := make([]byte, 44+len(audio))
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:16], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 8000)
	binary.LittleEndian.PutUint32(data[28:32], 16000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(len(audio)))
	return data
}

// syntheticMP3 is forty silent MPEG-1 Layer III frames at 128 kbps, 44.1 kHz.
func syntheticMP3() []byte {
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x64})
	return bytes.Repeat(frame, 40)
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
