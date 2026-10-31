package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPolicyCommandsStageOverrides(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "policy-cli-account")
	requirements.NoError(err)
	_, err = st.EnsureConversation(source.ID, "room", "Synthetic room")
	requirements.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.ConversationPolicy.Overrides = []conversationpolicy.Override{{SourceType: "beeper", SourceIdentifier: "policy-cli-account", ConversationID: "room", Tier: conversationpolicy.Full}}
	deps := conversationPolicyDeps{
		open:   func(context.Context) (*store.Store, func(), error) { return st, func() {}, nil },
		config: func(context.Context) *config.Config { return cfg },
	}
	run := func(args ...string) (string, error) {
		cmd := newPolicyLocalCmd(deps)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	out, err := run("set", "1", "room", "cold")
	requirements.NoError(err)
	assertions.Contains(out, "staged")
	out, err = run("list")
	requirements.NoError(err)
	assertions.Contains(out, `"effective_tier":"full"`)
	assertions.Contains(out, `"selected_override":"cold"`)
	assertions.Contains(out, `"override_origin":"cli"`)
	_, err = run("set", "1", "room", "stub")
	requirements.ErrorContains(err, "not enabled")
	_, err = run("set", "1", "room", "unknown")
	requirements.ErrorContains(err, "invalid conversation tier")
	_, err = run("reset", "1", "room")
	requirements.NoError(err)
	out, err = run("list")
	requirements.NoError(err)
	assertions.Contains(out, `"selected_override":"full"`)
	assertions.Contains(out, `"override_origin":"config"`)
	_, err = run("set", "--", "-1", "room", "cold")
	requirements.ErrorContains(err, "SOURCE_ID must be a positive integer")
}

func TestPolicyCommandRoutesToDaemon(t *testing.T) {
	assertions := assert.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assertions.Equal([]string{"policy", "set", "1", "room", "cold"}, req.Args)
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	cmd := &cobra.Command{Use: "msgvault"}
	cmd.AddCommand(newPolicyCmd())
	cmd.SetArgs([]string{"policy", "set", "1", "room", "cold"})
	require.NoError(t, cmd.ExecuteContext(ctx))
	assertions.Equal(1, int(requests.Load()))
}
