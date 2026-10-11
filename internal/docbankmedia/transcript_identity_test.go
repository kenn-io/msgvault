package docbankmedia

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type transcriptTransport func(*http.Request) (*http.Response, error)

func (f transcriptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type transcriptBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *transcriptBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *transcriptBody) Close() error { b.closed = true; return nil }

func TestTranscriptIdentityBoundsResponseBeforeUnits(t *testing.T) {
	const prefix = `{"$schema":"http://127.0.0.1/schemas/MediaTranscript.json","vault_uid":"vault","source_id":"source","source_version_id":"version","content_version_id":"content","evidence_state":"ready","coverage_state":"transcribed","operation_state":"succeeded","transcript":{`
	units := `"units":[{"text":"` + strings.Repeat("x", 8<<20) + `"}]}}`
	for _, tc := range []struct {
		name, metadata, origin string
	}{
		{"supplied", `"build_id":"build","supplied_input_id":"input","origin":"supplied",`, "supplied"},
		{"generated", `"build_id":"build","origin":"generated",`, "generated"},
		{"missing build", `"supplied_input_id":"input","origin":"supplied",`, ""},
		{"missing supplied input", `"build_id":"build","origin":"supplied",`, ""},
		{"units before provenance", ``, ""},
		{"oversized metadata", `"build_id":"build","origin":"generated","completeness":"` + strings.Repeat("x", maxMetadataBytes) + `",`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &transcriptBody{reader: strings.NewReader(prefix + tc.metadata + units)}
			client, err := NewClient("http://127.0.0.1", func() (string, error) { return "synthetic-key", nil })
			require.NoError(t, err)
			client.http = &http.Client{Transport: transcriptTransport(func(request *http.Request) (*http.Response, error) {
				assert.Equal(t, "/api/v1/media/sources/source/versions/version/transcript", request.URL.Path)
				assert.Equal(t, "content", request.URL.Query().Get("content_version_id"))
				return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
			})}
			identity, err := client.TranscriptIdentity(t.Context(), "source", "version", "content")
			assert.LessOrEqual(t, body.read, maxMetadataBytes)
			assert.True(t, body.closed)
			if tc.origin == "" {
				require.ErrorIs(t, err, ErrEvidenceUnavailable)
				assert.Nil(t, identity.Transcript)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, identity.Transcript)
			assert.Equal(t, "build", identity.Transcript.BuildID)
			assert.Equal(t, tc.origin, identity.Transcript.Origin)
			assert.Empty(t, identity.Transcript.Units)
		})
	}
}

func TestTranscriptIdentityRejectsReorderedSource(t *testing.T) {
	_, err := readTranscriptIdentity(strings.NewReader(`{"source_id":"source","vault_uid":"vault","source_version_id":"version","content_version_id":"content","evidence_state":"ready","coverage_state":"transcribed","operation_state":"succeeded","transcript":{"build_id":"build","origin":"generated","units":[]}}`))
	require.ErrorIs(t, err, ErrEvidenceUnavailable)
}
