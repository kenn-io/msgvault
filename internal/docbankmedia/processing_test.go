package docbankmedia_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/testutil/docbanktest"
)

func TestProcessingPlansAndProfiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fake := docbanktest.NewProcessingHandler(t)
	server := httptest.NewServer(fake)
	defer server.Close()
	client, err := docbankmedia.NewClient(server.URL, nil)
	require.NoError(err)
	profile, err := client.Profile(t.Context(), "documents")
	require.NoError(err)
	assert.Equal("profile-one", profile.Fingerprint)
	assert.True(profile.Rendition)
	_, err = client.Profile(t.Context(), "missing")
	require.ErrorContains(err, "document_profile")
	require.ErrorContains(err, "missing")
	fake.Rendition = false
	profile, err = client.Profile(t.Context(), "documents")
	require.NoError(err)
	assert.False(profile.Rendition)
	selector := docbankmedia.ProcessingSelector{NodeID: 7, ContentVersionID: "version", Profile: "documents"}
	plan, err := client.PlanProcessing(t.Context(), selector)
	require.NoError(err)
	assert.Equal("plan-one", plan.Fingerprint)
	jobID, err := client.StartProcessing(t.Context(), selector, plan.Fingerprint)
	require.NoError(err)
	assert.Equal("job-version", jobID)
	assert.Equal([]string{"version"}, fake.Plans)
	assert.Equal(int64(7), fake.NodeIDs["version"])
	assert.Equal([]string{"version"}, fake.Jobs)
}

func TestStartProcessingReturnsBeforeStreamEnds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := io.WriteString(w, "{\"type\":\"job\",\"job\":{\"id\":\"durable-job\"}}\n")
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
	assert.Equal(t, "durable-job", jobID)
}

func TestProcessingInvalidReceipts(t *testing.T) {
	for _, body := range []string{`{}`, `{"type":"done"}`, `{"type":"job","job":{}}`, `broken`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			defer server.Close()
			client, err := docbankmedia.NewClient(server.URL, nil)
			require.NoError(t, err)
			_, err = client.StartProcessing(t.Context(), docbankmedia.ProcessingSelector{NodeID: 7}, "plan")
			assert.ErrorIs(t, err, docbankmedia.ErrInvalidReceipt)
		})
	}
}
