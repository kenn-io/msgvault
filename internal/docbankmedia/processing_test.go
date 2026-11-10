package docbankmedia_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/testutil/docbanktest"
)

func TestProfile(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fake := docbanktest.NewProcessingHandler(t)
	server := httptest.NewServer(fake)
	defer server.Close()
	client, err := docbankmedia.NewClient(server.URL, nil)
	require.NoError(err)
	profile, err := client.Profile(t.Context(), "documents")
	require.NoError(err)
	assert.Equal(strings.Repeat("a", 64), profile.Fingerprint)
	assert.True(profile.Rendition)
	_, err = client.Profile(t.Context(), "missing")
	require.ErrorContains(err, "not listed by Docbank")
	require.ErrorContains(err, "missing")
	fake.Rendition = false
	profile, err = client.Profile(t.Context(), "documents")
	require.NoError(err)
	assert.False(profile.Rendition)
}

func TestStartProcessingReturnsBeforeStreamEnds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := io.WriteString(w, `{"type":"job","job":{"id":"`+docbanktest.ProcessingJobID("durable-job", "profile")+`"}}`+"\n")
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := docbankmedia.NewClient(server.URL, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	jobID, err := client.StartProcessing(ctx, docbankmedia.ProcessingSelector{NodeID: 7, ContentVersionID: "version", Profile: "documents"}, "plan")
	require.NoError(t, err)
	assert.Equal(t, docbanktest.ProcessingJobID("durable-job", "profile"), jobID)
}

func TestProcessingInvalidReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		invalid    bool
	}{
		{name: "wrong event", body: `{"type":"done","job":{"id":"` + docbanktest.ProcessingJobID("a", "profile") + `"}}`, invalid: true},
		{name: "missing job id", body: `{"type":"job","job":{}}`, invalid: true},
		{name: "truncated", body: `{"type":"job"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, tc.body)
				assert.NoError(err)
			}))
			defer server.Close()
			client, err := docbankmedia.NewClient(server.URL, nil)
			require.NoError(err)
			_, err = client.StartProcessing(t.Context(), docbankmedia.ProcessingSelector{NodeID: 7}, "plan")
			require.Error(err)
			if tc.invalid {
				assert.ErrorIs(err, docbankmedia.ErrInvalidReceipt)
				return
			}
			require.NotErrorIs(err, docbankmedia.ErrInvalidReceipt)
			assert.True(docbankmedia.Retryable(err))
		})
	}
}
