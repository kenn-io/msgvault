package taskclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cronTestUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func cronTestJob() map[string]any {
	return map[string]any{"uid": cronTestUID, "project_id": 1, "definition_event_uid": cronTestUID, "definition": map[string]any{"version": 1, "kind": "job", "enabled": true, "trigger": map[string]any{"kind": "interval", "interval_seconds": 60}, "action": map[string]any{"kind": "notify", "recipient": "synthetic"}, "issue": map[string]any{"kind": "per-run", "parent_uid": cronTestUID, "title": "Review"}, "overlap": "allow", "catchup": "all"}}
}

func cronTestClient(t *testing.T, handler http.HandlerFunc) *KataClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := newLoopbackClient(t, server.URL, "synthetic", nil)
	transport.maxResponseBytes = 16 << 20 // recurrence must clamp an enlarged task transport.
	return &KataClient{transport: transport, project: "example", projectID: 1}
}

func TestCronCatalogMalformedNativeJobCannotImplyEmptyCertainty(t *testing.T) {
	job := cronTestJob()
	job["uid"] = "short-ref"
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "capabilities") {
			writeTestJSON(t, w, map[string]any{"project_uid": cronTestUID, "event_features": []string{"cron_v1"}})
			return
		}
		writeTestJSON(t, w, map[string]any{"jobs": []any{job}})
	})
	catalog, err := client.RecurrenceCatalog(t.Context(), "example")
	require.NoError(t, err)
	assert.True(t, catalog.Truncated, "malformed native jobs hide unknown family membership")
	assert.Empty(t, catalog.Jobs)
}

func TestCronCatalogMissingArrayIsNotCompleteEmpty(t *testing.T) {
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "capabilities") {
			writeTestJSON(t, w, map[string]any{"project_uid": cronTestUID, "event_features": []string{"cron_v1"}})
			return
		}
		writeTestJSON(t, w, map[string]any{})
	})
	_, err := client.RecurrenceCatalog(t.Context(), "example")
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestCronCatalogSkipsWrongProjectAndReportsPartial(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	wrong := cronTestJob()
	wrong["project_id"] = 2
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "capabilities") {
			writeTestJSON(t, w, map[string]any{"project_uid": cronTestUID, "event_features": []string{"cron_v1"}})
			return
		}
		writeTestJSON(t, w, map[string]any{"jobs": []any{wrong, cronTestJob()}})
	})
	catalog, err := client.RecurrenceCatalog(t.Context(), "example")
	require.NoError(err)
	assert.True(catalog.Truncated)
	require.Len(catalog.Jobs, 1)
	assert.Equal(cronTestUID, catalog.Jobs[0].UID)
}

func TestCronCatalogBoundsAndRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests []string
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if strings.HasSuffix(r.URL.Path, "capabilities") {
			writeTestJSON(t, w, map[string]any{"project_uid": cronTestUID, "event_features": []string{"cron_v1"}})
			return
		}
		jobs := make([]any, 101)
		for i := range jobs {
			job := cronTestJob()
			job["uid"] = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5%03d", i)
			jobs[i] = job
		}
		// The 101st job must never be inspected.
		jobs[100] = "unreadable job"
		writeTestJSON(t, w, map[string]any{"jobs": jobs})
	})
	catalog, err := client.RecurrenceCatalog(t.Context(), "example")
	require.NoError(err)
	require.Len(catalog.Jobs, 100)
	assert.True(catalog.Truncated)
	assert.Equal([]string{"GET /api/v1/projects/1/cron/capabilities", "GET /api/v1/projects/1/cron/jobs?include_deleted=true"}, requests)
}

func TestCronReadResponseCaps(t *testing.T) {
	for _, family := range []bool{false, true} {
		t.Run(strconv.FormatBool(family), func(t *testing.T) {
			client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "capabilities") {
					writeTestJSON(t, w, map[string]any{"project_uid": cronTestUID, "event_features": []string{"cron_v1"}})
					return
				}
				_, err := w.Write([]byte(strings.Repeat(" ", int(DefaultMaxResponseSize)+1)))
				assert.NoError(t, err)
			})
			var err error
			if family {
				_, err = client.GetTaskFamily(t.Context(), "example", cronTestUID)
			} else {
				_, err = client.RecurrenceCatalog(t.Context(), "example")
			}
			require.ErrorIs(t, err, ErrResponseTooLarge)
		})
	}
}

func TestCronReadHonorsCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-release })
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.RecurrenceCatalog(ctx, "example"); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		assert.Fail(t, "request did not stop after cancellation")
	}
}

func TestCronCapabilityRequiredBeforeCatalog(t *testing.T) {
	for _, capabilities := range []map[string]any{
		{"project_uid": cronTestUID, "event_features": []string{}},
		{"project_uid": "short", "event_features": []string{"cron_v1"}},
	} {
		calls := 0
		client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; writeTestJSON(t, w, capabilities) })
		_, err := client.RecurrenceCatalog(t.Context(), "example")
		require.Error(t, err)
		assert.Equal(t, 1, calls, "unsupported or unbound capabilities cannot trigger discovery")
	}
}

func TestCronFamilyRetainsAuthoritativeChildProject(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"issue":    map[string]any{"uid": cronTestUID, "project_id": 1, "project_uid": cronTestUID},
			"children": []any{map[string]any{"uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "project_id": 2, "project_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAX", "qualified_id": "other#child", "short_id": "child", "status": "open", "parent": map[string]any{"uid": cronTestUID, "project": "example"}}},
		})
	})
	family, err := client.GetTaskFamily(t.Context(), "example", cronTestUID)
	require.NoError(err)
	assert.Equal(int64(1), family.Parent.ProjectID)
	require.Len(family.Children, 1)
	child := family.Children[0]
	assert.Equal(int64(2), child.ProjectID)
	assert.Equal("01ARZ3NDEKTSV4RRFFQ69G5FAX", child.ProjectUID)
	assert.Equal("other", child.Project)
	assert.Equal("other#child", child.QualifiedRef)
	assert.Equal(cronTestUID, child.ParentUID)
}
