package taskclient

import (
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cronWriteTestJob(t *testing.T) map[string]any {
	t.Helper()
	job := cronTestJob()
	definition, ok := job["definition"].(map[string]any)
	if !assert.True(t, ok) {
		return job
	}
	definition["future_field"] = map[string]any{"preserve": "exactly"}
	return job
}

func TestGetCronJobChecksProjectAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(map[string]any)
		wantErr error
	}{
		{name: "matching job", change: func(map[string]any) {}},
		{name: "wrong project", change: func(job map[string]any) { job["project_id"] = 2 }, wantErr: ErrWrongProject},
		{name: "wrong identity", change: func(job map[string]any) { job["uid"] = "01ARZ3NDEKTSV4RRFFQ69G5FAW" }, wantErr: ErrInvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := cronWriteTestJob(t)
			job["name"] = "synthetic job"
			tc.change(job)
			client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/projects/1/cron/jobs/"+cronTestUID, r.URL.Path)
				writeTestJSON(t, w, map[string]any{"job": job})
			})
			got, err := client.GetCronJob(t.Context(), "example", cronTestUID)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, cronTestUID, got.UID)
			assert.Equal(t, "synthetic job", got.name)
			assert.Contains(t, string(got.definition), "future_field")
		})
	}
}

func TestSetCronJobEnabledPreservesUneditedDefinition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		job := cronWriteTestJob(t)
		job["name"] = "synthetic job"
		if r.Method == http.MethodPut {
			var body struct {
				Expected   string         `json:"expected_event_uid"`
				Actor      string         `json:"actor"`
				Name       string         `json:"name"`
				Definition map[string]any `json:"definition"`
			}
			if !assert.NoError(json.UnmarshalRead(r.Body, &body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal(cronTestUID, body.Expected)
			assert.Equal("msgvault", body.Actor)
			assert.Equal("synthetic job", body.Name)
			assert.Equal(false, body.Definition["enabled"])
			assert.Equal(map[string]any{"preserve": "exactly"}, body.Definition["future_field"])
			job["definition"] = body.Definition
		}
		writeTestJSON(t, w, map[string]any{"job": job})
	})
	job, err := client.GetCronJob(t.Context(), "example", cronTestUID)
	require.NoError(err)
	result, err := client.SetCronJobEnabled(t.Context(), "example", job, cronTestUID, false)
	require.NoError(err)
	assert.False(result.Enabled)
	assert.Equal(cronTestUID, result.ParentUID)
}

func TestCronJobEditableRejectsUnrepresentedTriggerFields(t *testing.T) {
	job := cronWriteTestJob(t)
	definition, ok := job["definition"].(map[string]any)
	require.True(t, ok)
	trigger, ok := definition["trigger"].(map[string]any)
	require.True(t, ok)
	trigger["future_trigger_option"] = true
	client := cronTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		job["name"] = "synthetic job"
		writeTestJSON(t, w, map[string]any{"job": job})
	})
	loaded, err := client.GetCronJob(t.Context(), "example", cronTestUID)
	require.NoError(t, err)
	assert.False(t, loaded.Editable())
}

func TestScheduleTombstoneUsesViewedEvent(t *testing.T) {
	for _, deleted := range []bool{true, false} {
		t.Run(strconv.FormatBool(deleted), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				job := cronWriteTestJob(t)
				job["name"] = "synthetic job"
				if r.Method == http.MethodGet && !deleted {
					job["deleted_at"] = "2026-11-08T00:00:00Z"
				}
				if r.Method != http.MethodGet {
					var body struct {
						Actor    string `json:"actor"`
						Expected string `json:"expected_event_uid"`
					}
					if !assert.NoError(json.UnmarshalRead(r.Body, &body)) {
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					assert.Equal("msgvault", body.Actor)
					assert.Equal(cronTestUID, body.Expected)
					if deleted {
						assert.Equal(http.MethodDelete, r.Method)
						job["deleted_at"] = "2026-11-08T00:00:00Z"
					} else {
						assert.Equal(http.MethodPost, r.Method)
						assert.True(strings.HasSuffix(r.URL.Path, "/restore"))
					}
				}
				writeTestJSON(t, w, map[string]any{"job": job})
			})
			job, err := client.GetCronJob(t.Context(), "example", cronTestUID)
			require.NoError(err)
			result, err := client.SetCronJobDeleted(t.Context(), "example", job, cronTestUID, deleted)
			require.NoError(err)
			assert.Equal(deleted, result.Deleted)
		})
	}
}

func TestEditCronJobPreservesUneditedFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := cronTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		job := cronWriteTestJob(t)
		job["name"] = "synthetic job"
		if r.Method == http.MethodPut {
			var body struct {
				Definition map[string]any `json:"definition"`
			}
			if !assert.NoError(json.UnmarshalRead(r.Body, &body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal(map[string]any{"preserve": "exactly"}, body.Definition["future_field"])
			job["definition"] = body.Definition
		}
		writeTestJSON(t, w, map[string]any{"job": job})
	})
	job, err := client.GetCronJob(t.Context(), "example", cronTestUID)
	require.NoError(err)
	settings := KataCronSettings{TriggerKind: "cron", Cron: "0 9 * * 1", Timezone: "UTC", OccurrenceTitle: "Updated", OccurrenceBody: "Notes", Recipient: "synthetic", Catchup: "latest"}
	updated, err := client.EditCronJob(t.Context(), "example", job, cronTestUID, settings)
	require.NoError(err)
	assert.Equal("Updated", updated.OccurrenceTitle)
	assert.Equal("0 9 * * 1", updated.Cron)
}
