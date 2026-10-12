package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInboxArchivedReadsClosedStdout(t *testing.T) {
	for _, kind := range []string{"candidates", "context"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := storetest.New(t)
			mid := f.CreateMessage("stdout-context")
			target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "stdout-context"}
			_, err := f.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()})
			requirements.NoError(err)
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), mid)
			requirements.NoError(err)
			_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "Synthetic stdout body")
			requirements.NoError(err)
			server := httptest.NewServer(api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
			defer server.Close()
			input, err := json.Marshal(inboxcontrol.ContextRequest{Target: target})
			requirements.NoError(err)
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInboxArchivedReadsClosedStdoutHelper$")
			child.Env = []string{"HOME=" + t.TempDir(), "GOMAXPROCS=2", "MSGVAULT_READ_TEST_SERVER=" + server.URL, "MSGVAULT_READ_TEST_KIND=" + kind, "MSGVAULT_READ_TEST_SOURCE_ID=" + strconv.FormatInt(f.Source.ID, 10), "MSGVAULT_READ_TEST_SOURCE_IDENTIFIER=" + f.Source.Identifier}
			child.Stdin = bytes.NewReader(input)
			reader, writer, err := os.Pipe()
			requirements.NoError(err)
			requirements.NoError(reader.Close())
			defer writer.Close()
			child.Stdout = writer
			var stderr bytes.Buffer
			child.Stderr = &stderr
			err = child.Run()
			requirements.NoError(err, stderr.String())
			assertions.Contains(stderr.String(), "archived-read-output-error")
			var read bool
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT is_read FROM messages WHERE id=?`), mid).Scan(&read))
			assertions.False(read)
		})
	}
}

func TestInboxArchivedReadsClosedStdoutHelper(t *testing.T) {
	requirements := require.New(t)

	server := os.Getenv("MSGVAULT_READ_TEST_SERVER")
	if server == "" {
		t.Skip("subprocess helper")
	}
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: os.Getenv("HOME"), Remote: config.RemoteConfig{URL: server, APIKey: "synthetic-owner-key", AllowInsecure: true}})
	command := newInboxCmd()
	ensureSilenceUsageWrapped(command)
	command.SetContext(ctx)
	if os.Getenv("MSGVAULT_READ_TEST_KIND") == "context" {
		command.SetArgs([]string{"context", "--request", "-"})
		command.SetIn(os.Stdin)
	} else {
		identifier := os.Getenv("MSGVAULT_READ_TEST_SOURCE_IDENTIFIER")
		command.SetArgs([]string{"candidates", "--source-id", os.Getenv("MSGVAULT_READ_TEST_SOURCE_ID"), "--source-type", "gmail", "--source-identifier", identifier, "--account-id", identifier, "--scope", "message"})
	}
	command.SetOut(os.Stdout)
	command.SetErr(io.Discard)
	err := command.Execute()
	_, outputErr := fmt.Fprintf(os.Stderr, "archived-read-command-result: %v\n", err)
	requirements.NoError(outputErr)
	requirements.Error(err)
	requirements.Contains(err.Error(), "write inbox")
	_, err = fmt.Fprintln(os.Stderr, "archived-read-output-error")
	requirements.NoError(err)
	// The test runner would write its summary to the deliberately closed stdout.
	os.Exit(0)
}
