package skills

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The installed workflow talks to a separate native Kata service. These HTTP
// fixtures exercise that stable external contract, including faults a real
// service should never produce, by executing the exact rendered Python reader.
func TestOneToOneScopedHistory(t *testing.T) {
	must := require.New(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("optional native-history workflow requires Python 3")
	}
	rendered, err := Render("test")
	must.NoError(err)
	var code string
	for _, skill := range rendered {
		if skill.Name == "msgvault-one-to-one" {
			_, rest, found := strings.Cut(skill.Content, "```python\n")
			must.True(found)
			code, _, found = strings.Cut(rest, "\n```")
			must.True(found)
		}
	}
	must.NotEmpty(code)
	const uid = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	const effective = "01HZNQ7VFPK1XGD8R5MABCD4EY"
	issue := map[string]any{"uid": uid, "project_id": 7, "revision": 4, "status": "closed"}
	input, err := json.Marshal(map[string]any{"issue": issue})
	must.NoError(err)

	for _, scenario := range []string{
		"effective-close", "effective-close-full-page", "denied", "unsupported", "reset", "wrong-subject",
		"foreign-event", "invalid-close", "missing-effective", "cursor-stall",
		"cap", "oversized", "redirect", "revision-changed", "reopened",
		"null-page", "null-event", "null-current",
	} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("Bearer synthetic-token", r.Header.Get("Authorization"))
				assert.Equal(http.MethodGet, r.Method)
				if r.URL.Path == "/api/v1/projects/7/issues/"+uid {
					if scenario == "null-current" {
						assert.NoError(json.NewEncoder(w).Encode(map[string]any{"issue": nil}))
						return
					}
					current := map[string]any{"uid": uid, "revision": 4, "status": "closed"}
					if scenario == "revision-changed" {
						current["revision"] = 5
					}
					if scenario == "reopened" {
						current["status"] = "open"
					}
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"issue": current}))
					return
				}
				calls++
				assert.Equal("/api/v1/projects/7/events", r.URL.Path)
				assert.Equal(uid, r.URL.Query().Get("issue"))
				assert.Equal("100", r.URL.Query().Get("limit"))
				if calls == 1 {
					assert.Equal("true", r.URL.Query().Get("include_effective_close_event_uid"))
				}
				if scenario == "denied" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if scenario == "redirect" {
					http.Redirect(w, r, "/must-not-follow", http.StatusFound)
					return
				}
				if scenario != "unsupported" {
					w.Header().Set("X-Kata-Events-Issue-Uid", uid)
				}
				if scenario == "wrong-subject" {
					w.Header().Set("X-Kata-Events-Issue-Uid", "another-issue")
				}
				if scenario == "null-page" {
					assert.NoError(json.NewEncoder(w).Encode(nil))
					return
				}
				if scenario == "oversized" {
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{
						"events": []map[string]any{{
							"issue_uid": uid, "event_uid": effective, "type": "issue.closed",
							"payload": map[string]any{"reason": "done", "evidence": []any{map[string]any{"type": "commit", "value": "synthetic-sha"}}},
						}}, "effective_close_event_uid": effective,
						"next_after_id": 0, "reset_required": false,
						"padding": strings.Repeat("x", 1048577),
					}))
					return
				}
				// A later-arriving close is deliberately not the effective close.
				events := []map[string]any{
					{"issue_uid": uid, "event_uid": effective, "type": "issue.closed", "payload": map[string]any{"reason": "done", "evidence": []any{map[string]any{"type": "commit", "value": "synthetic-sha"}}}},
					{"issue_uid": uid, "event_uid": "reopen", "type": "issue.reopened"},
					{"issue_uid": uid, "event_uid": "later-arrival", "type": "issue.closed", "payload": map[string]any{"reason": "wontfix"}},
				}
				if scenario == "foreign-event" {
					events[0]["issue_uid"] = "another-issue"
				}
				if scenario == "null-event" {
					events[0] = nil
				}
				if scenario == "invalid-close" {
					events[0]["type"] = "issue.updated"
				}
				if scenario == "missing-effective" {
					events = events[1:]
				}
				if scenario == "cap" || scenario == "cursor-stall" {
					events = make([]map[string]any, 0, 100)
					for len(events) < 100 {
						events = append(events, map[string]any{
							"issue_uid": uid, "event_uid": strconv.Itoa(len(events)), "type": "issue.updated",
						})
					}
				}
				if scenario == "effective-close-full-page" {
					for len(events) < 100 {
						events = append(events, map[string]any{
							"issue_uid": uid, "event_uid": strconv.Itoa(len(events)), "type": "issue.updated",
						})
					}
				}
				next := calls * 100
				if scenario == "cursor-stall" {
					next = 0
				}
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{
					"events": events, "effective_close_event_uid": effective,
					"next_after_id": next, "reset_required": scenario == "reset",
				}))
			}))
			t.Cleanup(server.Close)
			path := filepath.Join(t.TempDir(), "issue.json")
			require.NoError(os.WriteFile(path, input, 0o600))
			command := exec.CommandContext(t.Context(), python, "-", path)
			command.Stdin = strings.NewReader(code)
			command.Env = append(os.Environ(), "KATA_SERVER="+server.URL, "KATA_AUTH_TOKEN=synthetic-token")
			output, runErr := command.CombinedOutput()
			if scenario == "effective-close" || scenario == "effective-close-full-page" {
				require.NoError(runErr, "%s", output)
				var result struct {
					Close struct {
						UID string `json:"event_uid"`
					} `json:"close"`
				}
				require.NoError(json.Unmarshal(output, &result))
				assert.Equal(effective, result.Close.UID)
				assert.Contains(string(output), "synthetic-sha")
				if scenario == "effective-close-full-page" {
					assert.Equal(1, calls)
				}
			} else {
				require.Error(runErr)
				assert.Contains(string(output), "Closure history unavailable:")
				assert.NotContains(string(output), "synthetic-token")
				if scenario == "oversized" {
					assert.Contains(string(output), "response budget exceeded")
				}
				switch scenario {
				case "cap":
					assert.Contains(string(output), "history exceeds 500-event budget")
				case "cursor-stall":
					assert.Contains(string(output), "history cursor did not advance")
				}
			}
			assert.LessOrEqual(calls, 5)
		})
	}
}
