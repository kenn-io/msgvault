package personagenda

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"hegel.dev/go/hegel"
)

// The hosted service never starts its scheduler. This proves the actual catalog
// and issue-family wire contracts without activating a schedule or notification.
func TestRecurringParentBeforeFirstDeliveryAgainstKata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	native := katatest.NewHosted(t, "msgvault")
	server := httptest.NewTLSServer(native.Handler())
	t.Cleanup(server.Close)
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, HTTPClient: server.Client(), DefaultProject: "msgvault"})
	require.NoError(err)
	parent, err := client.CreateTask(t.Context(), "msgvault", "recurring-parent", taskclient.KataCreate{Title: "Prepare weekly notes", Metadata: map[string]any{PersonMetadataKey: "retired-person", ListMetadataKey: "work"}})
	require.NoError(err)
	for i := range 2 {
		body := fmt.Sprintf(`{"actor":"msgvault","name":"notes-%d","definition":{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":3600,"timezone":"UTC"},"action":{"kind":"notify","recipient":"synthetic"},"issue":{"kind":"per-run","parent_uid":%q,"title":"Review notes"},"overlap":"allow","catchup":"latest"}}`, i, parent.UID)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("%s/api/v1/projects/%d/cron/jobs/01ARZ3NDEKTSV4RRFFQ69G5FA%d", server.URL, native.ProjectID, i), strings.NewReader(body))
		require.NoError(err)
		req.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(req)
		require.NoError(err)
		require.NoError(response.Body.Close())
		require.Contains([]int{http.StatusOK, http.StatusCreated}, response.StatusCode)
	}
	agenda := Service{Tasks: client, People: integrationPeople{1: {"person", "retired-person"}}, Project: "msgvault"}
	for _, closed := range []bool{false, true} {
		if closed {
			native.Endpoint(server).CloseIssue(t, parent.UID)
		}
		result, err := agenda.List(t.Context(), 1)
		require.NoError(err)

		require.Len(result.Items, 1, "closed parents remain series summaries")
		assert.Equal(parent.UID, result.Items[0].UID)
		assert.Equal("series", result.Items[0].Kind)
		assert.Equal("direct", result.Items[0].Membership)
		require.Len(result.Items[0].Schedules, 2, "one summary for multiple jobs")
		wantState := "paused"
		if closed {
			wantState = "parent_closed"
		}
		assert.Equal(wantState, result.Items[0].Schedules[0].State)
		assert.Equal("ready", result.Recurrence.State)
	}
}

const (
	fixtureProjectUID  = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	fixtureParentUID   = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
	fixtureJobUID      = "01ARZ3NDEKTSV4RRFFQ69G5FAB"
	fixtureEventUID    = "01ARZ3NDEKTSV4RRFFQ69G5FAC"
	fixtureOldEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FAD"
	fixtureChildUID    = "01ARZ3NDEKTSV4RRFFQ69G5FAE"
)

type recurrenceWire struct {
	jobs     []map[string]any
	families map[string]map[string]any
	ordinary []map[string]any
	failures map[string]int
	requests []string
}

func wireTask(uid, person string) map[string]any {
	metadata := map[string]any{}
	if person != "" {
		metadata[PersonMetadataKey] = person
	}
	return map[string]any{"uid": uid, "short_id": uid, "qualified_id": "msgvault#" + uid, "project_id": int64(1), "project_uid": fixtureProjectUID, "title": uid, "status": "open", "revision": int64(1), "metadata": metadata}
}
func wireParent(uid, person string) map[string]any {
	task := wireTask(uid, person)
	delete(task, "qualified_id") // issue-show parent uses ShowIssueOut, unlike child/list rows.
	return task
}
func wireJob(uid, parent string) map[string]any {
	return map[string]any{"uid": uid, "project_id": int64(1), "definition_event_uid": fixtureEventUID, "definition": map[string]any{"version": 1, "kind": "job", "enabled": true, "trigger": map[string]any{"kind": "cron", "cron": "0 9 * * 1", "timezone": "UTC"}, "action": map[string]any{"kind": "notify", "recipient": "synthetic"}, "issue": map[string]any{"kind": "per-run", "parent_uid": parent, "title": "Review"}, "overlap": "allow", "catchup": "latest"}}
}
func wireOccurrence(t *testing.T, uid, parent, job, person string) map[string]any {
	t.Helper()
	task := wireTask(uid, person)
	task["parent"] = map[string]any{"uid": parent, "project": "msgvault", "qualified_id": "msgvault#" + parent}
	metadata, ok := task["metadata"].(map[string]any)
	require.True(t, ok, "fixture metadata must be an object")
	metadata["cron.occurrence"] = map[string]any{"version": 1, "job_uid": job, "definition_event_uid": fixtureOldEventUID, "scheduled_at": "2026-11-01T12:00:00+02:00"}
	return task
}
func newRecurrenceWire(t *testing.T) *recurrenceWire {
	t.Helper()
	parent := wireParent(fixtureParentUID, "alias")
	metadata, ok := parent["metadata"].(map[string]any)
	require.True(t, ok, "fixture metadata must be an object")
	metadata[ListMetadataKey] = "work"
	return &recurrenceWire{jobs: []map[string]any{wireJob(fixtureJobUID, fixtureParentUID)}, families: map[string]map[string]any{fixtureParentUID: {"issue": parent, "children": []map[string]any{}}}, failures: map[string]int{}}
}
func (f *recurrenceWire) service(t *testing.T, uids ...string) Service {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		if !assert.Equal(t, http.MethodGet, r.Method, "recurrence reads must never mutate Kata") {
			http.Error(w, "unexpected fixture method", http.StatusMethodNotAllowed)
			return
		}
		if code := f.failures[r.URL.Path]; code != 0 {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case r.URL.Path == "/api/v1/health":
			body = map[string]any{"api_schema_version": "0.41.0"}
		case r.URL.Path == "/api/v1/projects":
			body = map[string]any{"projects": []map[string]any{{"id": 1, "uid": fixtureProjectUID, "name": "msgvault", "active": true}}}
		case strings.HasSuffix(r.URL.Path, "/cron/capabilities"):
			body = map[string]any{"project_uid": fixtureProjectUID, "event_features": []string{"cron_v1"}}
		case strings.HasSuffix(r.URL.Path, "/cron/jobs"):
			body = map[string]any{"jobs": f.jobs}
		case r.URL.Path == "/api/v1/projects/1/issues":
			// Return matching ordinary rows, as Kata's exact metadata filter does.
			var rows []map[string]any
			uid := strings.TrimPrefix(r.URL.Query().Get("meta"), PersonMetadataKey+"=")
			for _, task := range f.ordinary {
				metadata, ok := task["metadata"].(map[string]any)
				if !assert.True(t, ok, "fixture metadata must be an object") {
					http.Error(w, "invalid fixture metadata", http.StatusInternalServerError)
					return
				}
				if metadata[PersonMetadataKey] == uid {
					rows = append(rows, task)
				}
			}
			body = map[string]any{"issues": rows}
		default:
			uid := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/1/issues/")
			family, ok := f.families[uid]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body = family
		}
		encoded, err := json.Marshal(body)
		if !assert.NoError(t, err) {
			http.Error(w, "invalid fixture response", http.StatusInternalServerError)
			return
		}
		_, err = w.Write(encoded)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, HTTPClient: server.Client(), DefaultProject: "msgvault"})
	require.NoError(t, err)
	return Service{Tasks: client, People: integrationPeople{1: uids}, Project: "msgvault"}
}

func TestRecurrenceHistoricalEventsAndMembership(t *testing.T) {
	for _, tt := range []struct {
		name             string
		modify           func(map[string]any)
		included         bool
		membership, list string
	}{
		{"inherited historical event", func(map[string]any) {}, true, "inherited", "work"},
		{"explicit alias", func(c map[string]any) {
			fixtureMetadata(t, c["metadata"])[PersonMetadataKey] = "person"
			fixtureMetadata(t, c["metadata"])[ListMetadataKey] = "personal"
		}, true, "direct", "personal"},
		{"other person", func(c map[string]any) { fixtureMetadata(t, c["metadata"])[PersonMetadataKey] = "other" }, false, "", ""},
		{"null explicit person", func(c map[string]any) { fixtureMetadata(t, c["metadata"])[PersonMetadataKey] = nil }, false, "", ""},
		{"empty explicit person", func(c map[string]any) { fixtureMetadata(t, c["metadata"])[PersonMetadataKey] = "" }, false, "", ""},
		{"wrong parent", func(c map[string]any) { fixtureMetadata(t, c["parent"])["uid"] = fixtureChildUID }, false, "", ""},
		{"cross project", func(c map[string]any) {
			c["project_id"] = 2
			c["project_uid"] = "01ARZ3NDEKTSV4RRFFQ69G5FAF"
			c["qualified_id"] = "other#child"
		}, false, "", ""},
		{"wrong job", func(c map[string]any) {
			fixtureMetadata(t, fixtureMetadata(t, c["metadata"])["cron.occurrence"])["job_uid"] = fixtureChildUID
		}, false, "", ""},
		{"bad event", func(c map[string]any) {
			fixtureMetadata(t, fixtureMetadata(t, c["metadata"])["cron.occurrence"])["definition_event_uid"] = "short"
		}, false, "", ""},
		{"bad version", func(c map[string]any) {
			fixtureMetadata(t, fixtureMetadata(t, c["metadata"])["cron.occurrence"])["version"] = 2
		}, false, "", ""},
		{"bad instant", func(c map[string]any) {
			fixtureMetadata(t, fixtureMetadata(t, c["metadata"])["cron.occurrence"])["scheduled_at"] = "tomorrow"
		}, false, "", ""},
		{"closed child", func(c map[string]any) { c["status"] = "closed" }, false, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newRecurrenceWire(t)
			child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")
			tt.modify(child)
			f.families[fixtureParentUID]["children"] = []map[string]any{child, child}
			result, err := f.service(t, "person", "alias").List(t.Context(), 1)
			require.NoError(err)
			require.NotEmpty(result.Items)
			assert.Equal("series", result.Items[0].Kind)
			expected := 0
			if tt.included {
				expected = 1
			}
			require.Len(result.Items, expected+1)
			assert.Equal(expected, result.Items[0].OpenOccurrences)
			if tt.included {
				got := result.Items[1]
				assert.Equal("occurrence", got.Kind)
				assert.Equal(tt.membership, got.Membership)
				assert.Equal(tt.list, got.List)
				assert.Equal(fixtureParentUID, got.ParentUID)
				require.NotNil(got.Occurrence)
				assert.Equal(fixtureOldEventUID, got.Occurrence.DefinitionEventUID, "editing a job does not invalidate its historical children")
				assert.Equal("2026-11-01T10:00:00Z", got.Occurrence.ScheduledAt)
			}
		})
	}
}

func TestRecurrenceFailurePreservesOrdinaryTasks(t *testing.T) {
	for _, tt := range []struct {
		code  int
		state string
	}{{401, "authentication_required"}, {403, "forbidden"}, {404, "unsupported"}, {500, "unavailable"}} {
		t.Run(tt.state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newRecurrenceWire(t)
			f.ordinary = []map[string]any{wireTask("ordinary", "person")}
			f.failures["/api/v1/projects/1/cron/capabilities"] = tt.code
			result, err := f.service(t, "person", "alias").List(t.Context(), 1)
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("ordinary", result.Items[0].UID)
			assert.Equal("task", result.Items[0].Kind)
			assert.Equal("direct", result.Items[0].Membership)
			assert.Equal(tt.state, result.Recurrence.State)
		})
	}
}

func TestRecurrenceBoundsAndRoundRobin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	for family := range 3 {
		parent := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", family)
		job := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5E%02d", family)
		f.jobs = append(f.jobs, wireJob(job, parent))
		var children []map[string]any
		for n := range 110 {
			children = append(children, wireOccurrence(t, fmt.Sprintf("child-%d-%03d", family, n), parent, job, ""))
		}
		f.families[parent] = map[string]any{"issue": wireParent(parent, "alias"), "children": children}
	}
	f.ordinary = []map[string]any{wireTask("ordinary", "person")}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 100)
	assert.True(result.Truncated)
	counts := map[string]int{}
	for _, item := range result.Items {
		counts[item.Kind]++
		if item.Kind == "occurrence" {
			counts[item.ParentUID]++
		}
	}
	assert.Equal(4, counts["series"])
	assert.Equal(1, counts["task"])
	assert.Equal(32, counts["01ARZ3NDEKTSV4RRFFQ69G5F00"])
	assert.Equal(32, counts["01ARZ3NDEKTSV4RRFFQ69G5F01"])
	assert.Equal(31, counts["01ARZ3NDEKTSV4RRFFQ69G5F02"])
	for _, item := range result.Items {
		if item.Kind == "series" && item.UID != fixtureParentUID {
			assert.Equal(110, item.OpenOccurrences)
			assert.True(item.OccurrencesComplete)
		}
	}
}

func TestRecurrenceFamilyAndCatalogLimits(t *testing.T) {
	for _, n := range []int{25, 101} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			assert := assert.New(t)
			f := newRecurrenceWire(t)
			f.jobs = nil
			f.families = map[string]map[string]any{}
			for i := range n {
				parent := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5%03d", i)
				job := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G6%03d", i)
				f.jobs = append(f.jobs, wireJob(job, parent))
				f.families[parent] = map[string]any{"issue": wireParent(parent, "alias"), "children": []any{}}
			}
			result, err := f.service(t, "person", "alias").List(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal("partial", result.Recurrence.State)
			assert.True(result.Truncated)
			familyReads := 0
			for _, request := range f.requests {
				if strings.Contains(request, "/issues/") {
					familyReads++
				}
			}
			assert.Equal(24, familyReads)
			assert.Len(result.Items, 24)
		})
	}
}

// Explicit bindings always decide membership, for arbitrary metadata text;
// inheritance is permitted only when the key is absent, never null or empty.
func TestRecurrenceExplicitBindingProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		binding := hegel.Draw(ht, hegel.Text())
		mode := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))) % 5
		f := newRecurrenceWire(t)
		child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")
		metadata := fixtureMetadata(t, child["metadata"])
		included := false
		membership := "direct"
		switch mode {
		case 0:
			metadata[PersonMetadataKey] = binding
			included = binding == "person" || binding == "alias"
		case 1:
			metadata[PersonMetadataKey] = nil
		case 2:
			metadata[PersonMetadataKey] = "alias"
			included = true
		case 3:
			included = true
			membership = "inherited"
		case 4:
			metadata[PersonMetadataKey] = []string{binding}
		}
		f.families[fixtureParentUID]["children"] = []map[string]any{child}
		result, err := f.service(t, "person", "alias").List(t.Context(), 1)
		require.NoError(ht, err)
		want := 1
		if included {
			want = 2
		}
		require.Len(ht, result.Items, want)
		wantUIDs := map[string]int{fixtureParentUID: 1}
		if included {
			wantUIDs[fixtureChildUID] = 1
		}
		gotUIDs := make(map[string]int)
		for _, item := range result.Items {
			gotUIDs[item.UID]++
		}
		assert.Equal(ht, wantUIDs, gotUIDs, "exact eligible identities, each appearing once")
		if included {
			assert.Equal(ht, membership, result.Items[1].Membership)
		}
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

func TestRecurrenceAliasAndDedupProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		// Draw the full integer domain and bound the materialized response.
		duplicates := 1 + hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))%40
		f := newRecurrenceWire(t)
		child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "alias")
		f.ordinary = []map[string]any{child}
		children := make([]map[string]any, int(duplicates))
		for i := range children {
			children[i] = child
		}
		f.families[fixtureParentUID]["children"] = children
		result, err := f.service(t, "person", "alias").List(t.Context(), 1)
		require.NoError(ht, err)
		require.Len(ht, result.Items, 2)
		gotUIDs := make(map[string]int)
		for _, item := range result.Items {
			gotUIDs[item.UID]++
		}
		assert.Equal(ht, map[string]int{fixtureParentUID: 1, fixtureChildUID: 1}, gotUIDs, "aliases and duplicate source rows must preserve exact identities")
		assert.Equal(ht, 1, result.Items[0].OpenOccurrences)
		assert.Equal(ht, "occurrence", result.Items[1].Kind)
		assert.Equal(ht, "direct", result.Items[1].Membership)
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

func TestRecurrenceFailedFamilyIsPartial(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	f.ordinary = []map[string]any{wireTask("ordinary", "person")}
	f.failures["/api/v1/projects/1/issues/"+fixtureParentUID] = http.StatusForbidden
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	assert.Equal("partial", result.Recurrence.State)
	assert.True(result.Truncated)
	require.Len(result.Items, 1)
	assert.Equal("ordinary", result.Items[0].UID)
}

func TestRecurrenceNativeOnlyAndTombstones(t *testing.T) {
	for _, tt := range []struct {
		name   string
		modify func(map[string]any)
		want   string
	}{
		{"deleted", func(j map[string]any) { j["deleted_at"] = "2026-11-01T00:00:00Z" }, "deleted"},
		{"paused", func(j map[string]any) { fixtureMetadata(t, j["definition"])["enabled"] = false }, "paused"},
		{"adapter", func(j map[string]any) {
			fixtureMetadata(t, fixtureMetadata(t, j["definition"])["action"])["kind"] = "prompt"
		}, ""},
		{"completion engine", func(j map[string]any) {
			fixtureMetadata(t, fixtureMetadata(t, j["definition"])["trigger"])["kind"] = "issue-closed"
		}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newRecurrenceWire(t)
			tt.modify(f.jobs[0])
			f.families[fixtureParentUID]["children"] = []map[string]any{wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")}
			result, err := f.service(t, "person", "alias").List(t.Context(), 1)
			require.NoError(err)
			if tt.want == "" {
				assert.Empty(result.Items)
				return
			}
			require.Len(result.Items, 2)
			assert.Equal(tt.want, result.Items[0].Schedules[0].State)
			assert.Equal("occurrence", result.Items[1].Kind)
		})
	}
}

func TestRecurrenceDirectChildWithUnlinkedParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	fixtureMetadata(t, f.families[fixtureParentUID]["issue"])["metadata"] = map[string]any{PersonMetadataKey: "other-person", ListMetadataKey: "private list"}
	child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "person")
	fixtureMetadata(t, child["metadata"])[ListMetadataKey] = "my list"
	f.ordinary = []map[string]any{child}
	f.families[fixtureParentUID]["children"] = []map[string]any{child}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("occurrence", result.Items[0].Kind)
	assert.Equal("direct", result.Items[0].Membership)
	assert.Equal("my list", result.Items[0].List)
	assert.Empty(result.Items[0].Schedules, "unrelated parent details must not be exposed")
}

func TestRecurrenceCadenceSummaries(t *testing.T) {
	for _, tt := range []struct {
		kind, cron string
		seconds    int64
		want       string
	}{
		{"cron", "0 9 * * *", 0, "Daily at 09:00"},
		{"cron", "30 17 * * 1", 0, "Weekly on Monday at 17:30"},
		{"cron", "0 8 1 * *", 0, "Monthly on day 1 at 08:00"},
		{"cron", "0 9 * * 1-5", 0, "Weekdays at 09:00"},
		{"cron", "@hourly", 0, "Every hour"},
		{"cron", "15 9 1,15 * *", 0, "Custom schedule"},
		{"interval", "", 60, "Every minute"},
		{"interval", "", 120, "Every 2 minutes"},
		{"interval", "", 3600, "Every hour"},
		{"interval", "", 7200, "Every 2 hours"},
		{"interval", "", 86400, "Every day"},
		{"interval", "", 172800, "Every 2 days"},
		{"interval", "", 61, "Every 61 seconds"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			got := scheduleFromJob(taskclient.KataCronJob{TriggerKind: tt.kind, Cron: tt.cron, IntervalSeconds: tt.seconds}, false)
			assert.Equal(t, tt.want, got.Summary)
		})
	}
}

type ordinaryOnlyClient struct{ TaskClient }

func TestRecurrenceOptionalInterfacePreservesOrdinaryTasks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	f.ordinary = []map[string]any{wireTask("ordinary", "person")}
	service := f.service(t, "person")
	service.Tasks = ordinaryOnlyClient{service.Tasks}
	result, err := service.List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal("ordinary", result.Items[0].UID)
	assert.Equal("unsupported", result.Recurrence.State)
}

func TestRecurrenceAliasLimitKeepsOrdinaryTasks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	var uids []string
	for i := range 33 {
		uids = append(uids, fmt.Sprintf("alias-%d", i))
	}
	parent := fixtureMetadata(t, f.families[fixtureParentUID]["issue"])
	fixtureMetadata(t, parent["metadata"])[PersonMetadataKey] = uids[32]
	f.ordinary = []map[string]any{wireTask("ordinary", uids[32])}
	result, err := f.service(t, uids...).List(t.Context(), 1)
	require.NoError(err)
	assert.Equal("partial", result.Recurrence.State)
	assert.True(result.Truncated)
	require.Len(result.Items, 1)
	assert.Equal("ordinary", result.Items[0].UID)
}

func TestRecurrenceBudgetFairnessProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := 1 + int(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))%6)
		ordinary := int(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64))) % 111)
		f := newRecurrenceWire(t)
		f.jobs = nil
		f.families = map[string]map[string]any{}
		parents := make([]string, n)
		eligibleChildren := make(map[string]string)
		for family := range n {
			parent := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", family)
			job := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5E%02d", family)
			parents[family] = parent
			f.jobs = append(f.jobs, wireJob(job, parent))
			var children []map[string]any
			for i := range 110 {
				uid := fmt.Sprintf("child-%d-%03d", family, i)
				eligibleChildren[uid] = parent
				children = append(children, wireOccurrence(t, uid, parent, job, ""))
			}
			f.families[parent] = map[string]any{"issue": wireParent(parent, "alias"), "children": children}
		}
		for i := range ordinary {
			f.ordinary = append(f.ordinary, wireTask(fmt.Sprintf("ordinary-%03d", i), "person"))
		}
		result, err := f.service(t, "person", "alias").List(t.Context(), 1)
		require.NoError(ht, err)
		require.Len(ht, result.Items, 100)
		directBudget := min(ordinary, 100-n)
		occurrenceBudget := 100 - n - directBudget
		counts := map[string]int{}
		gotUIDs := make(map[string]int)
		for _, item := range result.Items {
			gotUIDs[item.UID]++
			assert.Equal(ht, 1, gotUIDs[item.UID], "returned UIDs must be unique")
			counts[item.Kind]++
			if item.Kind == "occurrence" {
				require.Contains(ht, eligibleChildren, item.UID, "only generated eligible children can enter the view")
				assert.Equal(ht, eligibleChildren[item.UID], item.ParentUID)
				counts[item.ParentUID]++
			}
		}
		assert.Equal(ht, n, counts["series"])
		assert.Equal(ht, directBudget, counts["task"])
		wantUIDs := make(map[string]int)
		for i := range directBudget {
			wantUIDs[fmt.Sprintf("ordinary-%03d", i)] = 1
		}
		for i, parent := range parents {
			want := occurrenceBudget / n
			if i < occurrenceBudget%n {
				want++
			}
			assert.Equal(ht, want, counts[parent])
			wantUIDs[parent] = 1
			for child := range want {
				wantUIDs[fmt.Sprintf("child-%d-%03d", i, child)] = 1
			}
		}
		assert.Equal(ht, wantUIDs, gotUIDs, "the independent budget model determines exact returned identities")
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

func TestRecurrenceEmptyExplicitBindingRegression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")
	fixtureMetadata(t, child["metadata"])[PersonMetadataKey] = ""
	f.families[fixtureParentUID]["children"] = []map[string]any{child}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(0, result.Items[0].OpenOccurrences)
	assert.False(result.Items[0].OccurrencesComplete)
}

func TestRecurrenceInheritedOccurrenceCanOwnSeries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	const nestedJob = "01ARZ3NDEKTSV4RRFFQ69G5FAF"
	const grandchild = "01ARZ3NDEKTSV4RRFFQ69G5FAG"
	child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")
	f.families[fixtureParentUID]["children"] = []map[string]any{child}
	f.jobs = append(f.jobs, wireJob(nestedJob, fixtureChildUID))
	childParent := wireParent(fixtureChildUID, "")
	childParent["metadata"] = child["metadata"]
	f.families[fixtureChildUID] = map[string]any{"issue": childParent, "children": []map[string]any{wireOccurrence(t, grandchild, fixtureChildUID, nestedJob, "")}}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	items := make(map[string]Item)
	for _, item := range result.Items {
		items[item.UID] = item
	}
	require.Len(items, 3)
	assert.Equal("series", items[fixtureChildUID].Kind)
	assert.Equal("inherited", items[fixtureChildUID].Membership)
	assert.Equal("work", items[fixtureChildUID].List)
	assert.Equal(fixtureChildUID, items[grandchild].ParentUID)
	assert.Equal("inherited", items[grandchild].Membership)
	assert.Equal("ready", result.Recurrence.State)
}

func TestRecurrenceNestedSeriesCannotInheritAcrossExplicitBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	const nestedJob = "01ARZ3NDEKTSV4RRFFQ69G5FAF"
	child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "other-person")
	f.families[fixtureParentUID]["children"] = []map[string]any{child}
	f.jobs = append(f.jobs, wireJob(nestedJob, fixtureChildUID))
	childParent := wireParent(fixtureChildUID, "other-person")
	childParent["metadata"] = child["metadata"]
	f.families[fixtureChildUID] = map[string]any{"issue": childParent, "children": []map[string]any{wireOccurrence(t, "grandchild", fixtureChildUID, nestedJob, "")}}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(fixtureParentUID, result.Items[0].UID)
}

func TestRecurrenceClosedInheritedSeriesRemainsVisible(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	const nestedJob = "01ARZ3NDEKTSV4RRFFQ69G5FAF"
	child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")
	child["status"] = "closed"
	f.families[fixtureParentUID]["children"] = []map[string]any{child}
	childParent := wireParent(fixtureChildUID, "")
	childParent["status"] = "closed"
	childParent["metadata"] = child["metadata"]
	f.jobs = append(f.jobs, wireJob(nestedJob, fixtureChildUID))
	f.families[fixtureChildUID] = map[string]any{"issue": childParent, "children": []map[string]any{}}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 2)
	for _, item := range result.Items {
		if item.UID == fixtureChildUID {
			require.Len(item.Schedules, 1)
			assert.Equal("parent_closed", item.Schedules[0].State)
			return
		}
	}
	assert.Fail("closed inherited series was not returned")
}

func TestRecurrenceUnsafeProofIsPartial(t *testing.T) {
	for _, tt := range []struct {
		name   string
		modify func(*recurrenceWire)
	}{
		{"malformed parent person", func(f *recurrenceWire) {
			fixtureMetadata(t, fixtureMetadata(t, f.families[fixtureParentUID]["issue"])["metadata"])[PersonMetadataKey] = nil
		}},
		{"malformed occurrence provenance", func(f *recurrenceWire) {
			child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")
			fixtureMetadata(t, child["metadata"])["cron.occurrence"] = nil
			f.families[fixtureParentUID]["children"] = []map[string]any{child}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			f := newRecurrenceWire(t)
			tt.modify(f)
			result, err := f.service(t, "person", "alias").List(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal("partial", result.Recurrence.State)
			assert.True(result.Truncated)
			for _, item := range result.Items {
				if item.Kind == "series" {
					assert.False(item.OccurrencesComplete)
				}
			}
		})
	}
}

func TestRecurrenceOccurrenceOrdering(t *testing.T) {
	f := newRecurrenceWire(t)
	var children []map[string]any
	for _, tt := range []struct{ uid, stamp string }{
		{"z", "2026-11-01T10:00:00.1Z"},
		{"b", "2026-11-01T10:00:00Z"},
		{"a", "2026-11-01T12:00:00+02:00"},
		{"c", "2026-11-01T09:00:00.999Z"},
	} {
		child := wireOccurrence(t, tt.uid, fixtureParentUID, fixtureJobUID, "")
		fixtureMetadata(t, fixtureMetadata(t, child["metadata"])["cron.occurrence"])["scheduled_at"] = tt.stamp
		children = append(children, child)
	}
	f.families[fixtureParentUID]["children"] = children
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, result.Items, 5)
	var got []string
	for _, item := range result.Items[1:] {
		got = append(got, item.UID)
	}
	assert.Equal(t, []string{"c", "a", "b", "z"}, got)
}

func TestRecurrenceCatalogAndAliasCapsMakeCountsIncomplete(t *testing.T) {
	for _, bound := range []string{"catalog", "aliases"} {
		t.Run(bound, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newRecurrenceWire(t)
			uids := []string{"person", "alias"}
			if bound == "catalog" {
				f.jobs = nil
				for i := range 101 {
					f.jobs = append(f.jobs, wireJob(fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5%03d", i), fixtureParentUID))
				}
			} else {
				for i := range 31 {
					uids = append(uids, fmt.Sprintf("extra-alias-%d", i))
				}
				f.families[fixtureParentUID]["children"] = []map[string]any{wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, uids[32])}
			}
			result, err := f.service(t, uids...).List(t.Context(), 1)
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal("series", result.Items[0].Kind)
			assert.False(result.Items[0].OccurrencesComplete)
			assert.Equal("partial", result.Recurrence.State)
			assert.True(result.Truncated)
		})
	}
}

func TestRecurrenceUnrelatedFamilyFailureKeepsVerifiedCountComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	f.jobs = append(f.jobs, wireJob("01ARZ3NDEKTSV4RRFFQ69G5FAF", "01ARZ3NDEKTSV4RRFFQ69G5FAG"))
	f.families[fixtureParentUID]["children"] = []map[string]any{wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "")}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal(1, result.Items[0].OpenOccurrences)
	assert.True(result.Items[0].OccurrencesComplete)
	assert.Equal("partial", result.Recurrence.State)
}

// Kata v0.22 recurringParent/initializeRecurringTx permits an issue carrying
// reserved occurrence provenance to own another native job. Scheduler writes
// remain upstream; this wire fixture exercises only the resulting read roles.
func TestRecurrenceSeriesWinsOccurrenceUIDCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRecurrenceWire(t)
	child := wireOccurrence(t, fixtureChildUID, fixtureParentUID, fixtureJobUID, "person")
	childJobUID := "01ARZ3NDEKTSV4RRFFQ69G5FAF"
	f.jobs = append(f.jobs, wireJob(childJobUID, fixtureChildUID))
	f.families[fixtureParentUID]["children"] = []map[string]any{child}
	childParent := wireParent(fixtureChildUID, "person")
	childParent["metadata"] = child["metadata"]
	f.families[fixtureChildUID] = map[string]any{"issue": childParent, "children": []any{}}
	f.ordinary = []map[string]any{child}
	result, err := f.service(t, "person", "alias").List(t.Context(), 1)
	require.NoError(err)
	require.Len(result.Items, 2, "one displayed row per UID across series and occurrence roles")
	summaries := map[string]Item{}
	for _, item := range result.Items {
		assert.Equal("series", item.Kind)
		summaries[item.UID] = item
	}
	require.Len(summaries, 2)
	require.Len(summaries[fixtureChildUID].Schedules, 1)
	assert.Equal(childJobUID, summaries[fixtureChildUID].Schedules[0].UID)
	assert.Equal(1, summaries[fixtureParentUID].OpenOccurrences, "role dedup must preserve the verified family count")
	assert.Equal(0, summaries[fixtureChildUID].OpenOccurrences)
	assert.True(summaries[fixtureParentUID].OccurrencesComplete)
	assert.False(result.Truncated, "the same UID already has its reserved series row")
}

func TestRecurrenceUIDRoleDedupProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		assert := assert.New(ht)
		require := require.New(ht)
		n := 2 + int(hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))%7)
		f := newRecurrenceWire(t)
		f.jobs = nil
		f.families = map[string]map[string]any{}
		parents := make([]string, n)
		jobs := make([]string, n)
		for i := range n {
			parents[i] = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", i)
			jobs[i] = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5E%02d", i)
		}
		for i := range n {
			f.jobs = append(f.jobs, wireJob(jobs[i], parents[i]))
			parent := wireParent(parents[i], "alias")
			if i > 0 {
				occurrence := wireOccurrence(t, parents[i], parents[i-1], jobs[i-1], "alias")
				parent["metadata"] = occurrence["metadata"]
				f.ordinary = append(f.ordinary, occurrence)
			}
			var children []map[string]any
			if i+1 < n {
				children = append(children, wireOccurrence(t, parents[i+1], parents[i], jobs[i], "alias"))
			}
			f.families[parents[i]] = map[string]any{"issue": parent, "children": children}
		}
		result, err := f.service(t, "person", "alias").List(t.Context(), 1)
		require.NoError(err)
		require.Len(result.Items, n)
		byUID := map[string]Item{}
		for _, item := range result.Items {
			assert.Equal("series", item.Kind)
			byUID[item.UID] = item
		}
		require.Len(byUID, n)
		for i, uid := range parents {
			item := byUID[uid]
			require.Len(item.Schedules, 1)
			assert.Equal(jobs[i], item.Schedules[0].UID)
			want := 1
			if i == n-1 {
				want = 0
			}
			assert.Equal(want, item.OpenOccurrences)
		}
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

func fixtureMetadata(t *testing.T, obj any) map[string]any {
	t.Helper()
	object, ok := obj.(map[string]any)
	require.True(t, ok, "fixture value must be an object")
	require.NotNil(t, object, "fixture object must not be null")
	return object
}
