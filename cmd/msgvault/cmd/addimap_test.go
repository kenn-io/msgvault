package cmd

import (
	"bytes"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPasswordPromptStrategy(t *testing.T) {
	tests := []struct {
		name       string
		stdinNat   bool // stdin is a native terminal
		stdinCyg   bool // stdin is a Cygwin/MSYS PTY
		stderrTTY  bool
		stdoutTTY  bool
		wantMethod passwordMethod
		wantOutput *os.File // nil for pipe/error methods
	}{
		{
			name:       "normal interactive terminal",
			stdinNat:   true,
			stderrTTY:  true,
			stdoutTTY:  true,
			wantMethod: passwordInteractive,
			wantOutput: os.Stderr,
		},
		{
			name:       "stdout redirected",
			stdinNat:   true,
			stderrTTY:  true,
			stdoutTTY:  false,
			wantMethod: passwordInteractive,
			wantOutput: os.Stderr,
		},
		{
			name:       "stderr redirected",
			stdinNat:   true,
			stderrTTY:  false,
			stdoutTTY:  true,
			wantMethod: passwordInteractive,
			wantOutput: os.Stdout,
		},
		{
			name:       "both outputs redirected, native stdin",
			stdinNat:   true,
			stderrTTY:  false,
			stdoutTTY:  false,
			wantMethod: passwordNoPrompt,
		},
		{
			name:       "cygwin normal terminal",
			stdinCyg:   true,
			stderrTTY:  true,
			stdoutTTY:  true,
			wantMethod: passwordInteractive,
			wantOutput: os.Stderr,
		},
		{
			name:       "cygwin stdout redirected",
			stdinCyg:   true,
			stderrTTY:  true,
			stdoutTTY:  false,
			wantMethod: passwordInteractive,
			wantOutput: os.Stderr,
		},
		{
			name:       "cygwin stderr redirected",
			stdinCyg:   true,
			stderrTTY:  false,
			stdoutTTY:  true,
			wantMethod: passwordInteractive,
			wantOutput: os.Stdout,
		},
		{
			name:       "cygwin both outputs redirected",
			stdinCyg:   true,
			stderrTTY:  false,
			stdoutTTY:  false,
			wantMethod: passwordNoPrompt,
		},
		{
			name:       "piped stdin",
			stdinNat:   false,
			stdinCyg:   false,
			stderrTTY:  true,
			stdoutTTY:  true,
			wantMethod: passwordPipe,
		},
		{
			name:       "piped stdin, all redirected",
			stdinNat:   false,
			stdinCyg:   false,
			stderrTTY:  false,
			stdoutTTY:  false,
			wantMethod: passwordPipe,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method, output := choosePasswordStrategy(
				tt.stdinNat, tt.stdinCyg, tt.stderrTTY, tt.stdoutTTY,
			)
			assert.Equal(t, tt.wantMethod, method, "method")
			assert.Equal(t, tt.wantOutput, output, "output")
		})
	}
}

func TestReadPasswordFromPipe(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:  "reads password from pipe",
			input: "secret123\n",
			want:  "secret123",
		},
		{
			name:  "trims trailing newline",
			input: "mypassword\n",
			want:  "mypassword",
		},
		{
			name:  "trims trailing CRLF",
			input: "mypassword\r\n",
			want:  "mypassword",
		},
		{
			name:  "handles no trailing newline",
			input: "mypassword",
			want:  "mypassword",
		},
		{
			name:    "rejects empty input",
			input:   "\n",
			wantErr: "password is required",
		},
		{
			name:    "rejects whitespace-only input",
			input:   "  \n",
			wantErr: "password is required",
		},
		{
			name:    "rejects EOF with no data",
			input:   "",
			wantErr: "password is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			r := strings.NewReader(tt.input)
			got, err := readPasswordFromPipe(r)
			if tt.wantErr != "" {
				requirements.Error(err, "expected error containing %q", tt.wantErr)
				requirements.ErrorContains(err, tt.wantErr)
				return
			}
			requirements.NoError(err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadPasswordFromPipeLargeInput(t *testing.T) {
	// Only first line should be used as the password.
	input := "firstline\nsecondline\n"
	r := strings.NewReader(input)
	got, err := readPasswordFromPipe(r)
	require.NoError(t, err)
	assert.Equal(t, "firstline", got)
}

// Verify the function signature accepts io.Reader.
var _ func(io.Reader) (string, error) = readPasswordFromPipe

func TestAddIMAPUsesDaemonRunnerAndForwardsPasswordEnv(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const host = "localhost"
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assertions.Equal([]string{
			"add-imap",
			"--archive-mailbox=Saved Mail",
			"--host=" + host,
			"--no-tls",
			"--port=1",
			"--username=alice@example.com",
		}, req.Args, "args")
		assertions.Equal(map[string]string{"MSGVAULT_IMAP_PASSWORD": "secret"}, req.Env, "env")
	}, `{"type":"stdout","data":"IMAP account added successfully!\n"}`, `{"type":"complete"}`)

	savedHost := imapHost
	savedPort := imapPort
	savedUsername := imapUsername
	savedNoTLS := imapNoTLS
	savedStartTLS := imapSTARTTLS
	savedNoDefaultIdentity := noDefaultIdentityAddImap
	t.Cleanup(func() {
		imapHost = savedHost
		imapPort = savedPort
		imapUsername = savedUsername
		imapNoTLS = savedNoTLS
		imapSTARTTLS = savedStartTLS
		noDefaultIdentityAddImap = savedNoDefaultIdentity
	})
	testCtx := configureRemoteDaemonForTest(t, server.URL)
	_ = testCtx
	t.Setenv("MSGVAULT_IMAP_PASSWORD", "secret")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := newAddIMAPCmd()
	cmd.SetContext(testCtx)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"--archive-mailbox", "Saved Mail",
		"--host", host,
		"--port", "1",
		"--username", "alice@example.com",
		"--no-tls",
	})

	requirements.NoError(cmd.Execute(), "add-imap")

	assertions.Equal(1, int(requests.Load()), "runner endpoint calls")
	assertions.Equal("IMAP account added successfully!\n", stdout.String(), "stdout")
	assertions.Contains(stderr.String(), "Using password from MSGVAULT_IMAP_PASSWORD", "stderr")
}

func TestAddIMAPArchiveMailboxSavedAcrossReauthorization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	savedHost, savedPort, savedUsername := imapHost, imapPort, imapUsername
	savedNoTLS, savedStartTLS, savedNoDefaultIdentity := imapNoTLS, imapSTARTTLS, noDefaultIdentityAddImap
	t.Cleanup(func() {
		imapHost, imapPort, imapUsername = savedHost, savedPort, savedUsername
		imapNoTLS, imapSTARTTLS, noDefaultIdentityAddImap = savedNoTLS, savedStartTLS, savedNoDefaultIdentity
	})
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("MSGVAULT_IMAP_PASSWORD", testutil.IMAPTestPassword)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0, "Saved Mail": 0})
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	identifier := (&imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}).Identifier()
	for _, step := range []struct {
		flags     []string
		want      string
		wantError bool
	}{
		{[]string{"--archive-mailbox", "Saved Mail"}, "Saved Mail", false},
		{[]string{"--archive-mailbox", "Missing Mail"}, "Saved Mail", true},
		{[]string{"--archive-mailbox", "INBOX"}, "Saved Mail", true},
		{nil, "Saved Mail", false},
		{[]string{"--archive-mailbox="}, "", false},
	} {
		cmd := newAddIMAPCmd()
		cmd.SetContext(ctx)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		args := []string{"--host", host, "--port", portText, "--username", testutil.IMAPTestUsername, "--no-tls", "--no-default-identity"}
		cmd.SetArgs(append(args, step.flags...))
		err := cmd.Execute()
		if step.wantError {
			requirements.Error(err)
			require.ErrorContains(t, err, "archive mailbox")
		} else {
			requirements.NoError(err)
		}
		path, err := cfg.DatabasePath()
		requirements.NoError(err)
		db, err := store.Open(path)
		requirements.NoError(err)
		source, err := db.GetSourceByTypeAndIdentifier("imap", identifier)
		requirements.NoError(err)
		saved, err := imapclient.ConfigFromJSON(source.SyncConfig.String)
		requirements.NoError(err)
		assertions.Equal(step.want, saved.ArchiveMailbox)
		requirements.NoError(db.Close())
	}
}
