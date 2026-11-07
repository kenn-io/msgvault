package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type fakeCallCommandClient struct {
	imports          [][]byte
	lists, summaries []store.CallQuery
}

func (c *fakeCallCommandClient) APISchemaVersion(context.Context) (string, error) {
	return callsMinAPIVersion, nil
}

func (c *fakeCallCommandClient) ImportCallObservation(_ context.Context, data []byte) (*generated.CallImportResponse, error) {
	c.imports = append(c.imports, data)
	return &generated.CallImportResponse{MessageID: 42, SourceID: 7}, nil
}

func (c *fakeCallCommandClient) ListCallObservations(_ context.Context, q store.CallQuery) (*generated.CallEvidenceResponse, error) {
	c.lists = append(c.lists, q)
	return &generated.CallEvidenceResponse{}, nil
}

func (c *fakeCallCommandClient) SummarizeCalls(_ context.Context, q store.CallQuery) (*generated.CallSummaryResponse, error) {
	c.summaries = append(c.summaries, q)
	return &generated.CallSummaryResponse{Items: []generated.PersonCallSummary{{PersonID: 23, Answered: 2, ExampleMessageID: 42}}}, nil
}

func TestCallsCommandThroughDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	client := &fakeCallCommandClient{}
	cleanups := 0
	run := func(input string, args ...string) (string, error) {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
			return client, func() { cleanups++ }, nil
		}})
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetIn(strings.NewReader(input))
		command.SetArgs(args)
		err := command.ExecuteContext(t.Context())
		return output.String(), err
	}
	const input = `{"format":"signal_desktop","account":"example","record":{"callId":"one","timestamp":1790841600000}}`
	output, err := run(input, "import", "-")
	requirements.NoError(err, output)
	assertions.Equal([][]byte{[]byte(input)}, client.imports)
	assertions.JSONEq(`{"message_id":42,"source_id":7}`, output)
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	before := after.AddDate(0, 1, 0)
	output, err = run("", "summary", "--person-id", "23", "--limit", "3", "--offset", "2", "--after", "2026-10-01T00:00:00Z", "--before", "2026-11-01T00:00:00Z")
	requirements.NoError(err, output)
	assertions.Equal([]store.CallQuery{{PersonID: 23, Limit: 3, Offset: 2, After: &after, Before: &before}}, client.summaries)
	assertions.JSONEq(`{"items":[{"person_id":23,"answered":2,"missed":0,"declined":0,"cancelled":0,"unknown":0,"example_message_id":42}]}`, output)
	output, err = run("", "list", "--person-id", "23", "--limit", "1", "--after-id", "42", "--after", "2026-10-01T00:00:00Z", "--before", "2026-11-01T00:00:00Z")
	requirements.NoError(err, output)
	assertions.Equal([]store.CallQuery{{PersonID: 23, Limit: 1, AfterID: 42, After: &after, Before: &before}}, client.lists)
	assertions.JSONEq(`{"items":[]}`, output)
	_, err = run("", "summary", "--after", "invalid")
	requirements.Error(err)
	assertions.Len(client.summaries, 1)
	assertions.Equal(3, cleanups, "invalid --after must fail before opening the daemon client")
}
