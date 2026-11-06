package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/timeline"
)

type timelineFakeClient struct {
	req    timeline.Request
	calls  int
	err    error
	result *timeline.Timeline
}

func (f *timelineFakeClient) APISchemaVersion(context.Context) (string, error) { return "3.47.0", nil }
func (f *timelineFakeClient) Timeline(_ context.Context, req timeline.Request) (*timeline.Timeline, string, error) {
	f.req = req
	f.calls++
	if f.result != nil {
		return f.result, "", f.err
	}
	return &timeline.Timeline{Mode: timeline.ModeAnchor}, "# Timeline\n", f.err
}
func TestTimelineCommandFlagsAndFormats(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	fake := &timelineFakeClient{}
	opens := 0
	timelineOpen = func(context.Context) (timelineClient, func(), error) {
		opens++
		return fake, func() {}, nil
	}
	for _, flag := range []string{"days", "budget"} {
		command := newTimelineCommand()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"--" + flag, "0"})
		requirements.ErrorContains(command.Execute(), "--"+flag+" must be between")
	}
	assertions.Zero(opens, "invalid explicit zeros fail before endpoint resolution")
	for _, flag := range []string{"with", "around", "date", "from", "to", "cursor"} {
		command := newTimelineCommand()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"--" + flag + "="})
		requirements.ErrorContains(command.Execute(), "--"+flag+" must not be empty")
	}
	assertions.Zero(opens, "empty selectors fail before endpoint resolution")
	command := newTimelineCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{"--with", "person:7", "--around", "msg:9", "--date", "2027-03-01", "--before", "2", "--after", "3", "--conversation", "--tier", "1,3", "--include-cold", "--include-noise", "--brief", "--budget", "4096", "--cursor", "next", "--json"})
	requirements.NoError(command.Execute())
	assertions.Equal(timeline.Request{With: "person:7", Around: "msg:9", Date: "2027-03-01", Before: 2, After: 3, BeforeSet: true, AfterSet: true, Conversation: true, Tiers: []int{1, 3}, IncludeCold: true, IncludeNoise: true, Brief: true, Budget: 4096, Cursor: "next", Format: "json"}, fake.req)
	assertions.True(strings.HasSuffix(out.String(), "\n"))
	command = newTimelineCommand()
	out.Reset()
	command.SetOut(&out)
	command.SetArgs(nil)
	requirements.NoError(command.Execute())
	assertions.Equal("# Timeline\n", out.String())
	assertions.Equal("markdown", fake.req.Format)
	assertions.Zero(fake.req.Before)
	assertions.Zero(fake.req.After)
	assertions.False(fake.req.BeforeSet)
	assertions.False(fake.req.AfterSet)
	command = newTimelineCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetArgs([]string{"--around", "msg:7", "--before", "0", "--after", "0"})
	requirements.NoError(command.Execute())
	assertions.Zero(fake.req.Before)
	assertions.Zero(fake.req.After)
	assertions.True(fake.req.BeforeSet)
	assertions.True(fake.req.AfterSet)
}

func TestTimelineCommandRefusesOldDaemon(t *testing.T) {
	unsetServerKeyEnvironmentForTest(t)
	oldSchemaCheck := remoteAPISchemaCheckEnabled
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = oldSchemaCheck })
	var timelineCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprint(w, `{"status":"ok","api_schema_version":"3.45.0"}`)
		assert.NoError(t, err)
	})
	mux.HandleFunc("/api/v1/timeline", func(w http.ResponseWriter, r *http.Request) {
		timelineCalls.Add(1)
		http.NotFound(w, r)
	})
	selected := httptest.NewServer(mux)
	t.Cleanup(selected.Close)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Remote = config.RemoteConfig{URL: selected.URL, APIKey: "synthetic-remote", AllowInsecure: true}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	command := newTimelineCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs(nil)
	require.EqualError(t, command.ExecuteContext(ctx), `timeline requires daemon API schema 3.47.0 or newer (daemon reports "3.45.0"); upgrade and restart the daemon`)
	assert.Zero(t, timelineCalls.Load())
}

func TestTimelineCommandReencodesJSON(t *testing.T) {
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	fake := &timelineFakeClient{result: &timeline.Timeline{Mode: timeline.ModeAnchor, History: []timeline.Entry{
		{Kind: "message", Message: &timeline.MessageEntry{Snippet: timeline.UntrustedText{Text: "\"\\ <é🙂>"}}},
	}}}
	timelineOpen = func(context.Context) (timelineClient, func(), error) { return fake, func() {}, nil }
	var out bytes.Buffer
	command := newTimelineCommand()
	command.SetOut(&out)
	command.SetArgs([]string{"--json"})
	require.NoError(t, command.Execute())
	assert.Contains(t, out.String(), `"text":"\"\\ <é🙂>"`)
	assert.True(t, strings.HasSuffix(out.String(), "\n"))
}

func TestTimelineCommandPrintsAmbiguousReferences(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	fake := &timelineFakeClient{err: &timeline.ErrAmbiguousReference{Candidates: []timeline.Candidate{
		{PersonID: 7, ParticipantIDs: []int64{8}, DisplayName: "Alex\x1b[31m", Address: "saved@example.com"},
		{ParticipantIDs: []int64{9}, DisplayName: "Alex\x1b[31m", Address: "unsaved@example.com"},
	}}}
	timelineOpen = func(context.Context) (timelineClient, func(), error) { return fake, func() {}, nil }
	var candidates, output bytes.Buffer
	command := newTimelineCommand()
	command.SilenceUsage = true
	command.SetErr(&candidates)
	command.SetOut(&output)
	command.SetArgs([]string{"--with", "name:Alex", "--json"})
	var ambiguous *timeline.ErrAmbiguousReference
	requirements.ErrorAs(command.Execute(), &ambiguous)
	assertions.Equal("person:7	Alex	saved@example.com\nparticipant:9	Alex	unsaved@example.com\nError: more than one person matches; choose a match\n", candidates.String())
	assertions.Empty(output.String())
}
