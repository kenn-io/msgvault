package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
			assertpkg.Equal(t, tt.wantMethod, method, "method")
			assertpkg.Equal(t, tt.wantOutput, output, "output")
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
			require := requirepkg.New(t)
			r := strings.NewReader(tt.input)
			got, err := readPasswordFromPipe(r)
			if tt.wantErr != "" {
				require.Error(err, "expected error containing %q", tt.wantErr)
				require.ErrorContains(err, tt.wantErr)
				return
			}
			require.NoError(err)
			assertpkg.Equal(t, tt.want, got)
		})
	}
}

func TestReadPasswordFromPipeLargeInput(t *testing.T) {
	// Only first line should be used as the password.
	input := "firstline\nsecondline\n"
	r := strings.NewReader(input)
	got, err := readPasswordFromPipe(r)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, "firstline", got)
}

// Verify the function signature accepts io.Reader.
var _ func(io.Reader) (string, error) = readPasswordFromPipe

func TestAddIMAPUsesDaemonRunnerAndForwardsPasswordEnv(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	const host = "localhost"
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"add-imap",
			"--host=" + host,
			"--no-tls",
			"--port=1",
			"--username=alice@example.com",
		}, req.Args, "args")
		assert.Equal(map[string]string{"MSGVAULT_IMAP_PASSWORD": "secret"}, req.Env, "env")
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
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv("MSGVAULT_IMAP_PASSWORD", "secret")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := newAddIMAPCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"--host", host,
		"--port", "1",
		"--username", "alice@example.com",
		"--no-tls",
	})

	require.NoError(cmd.Execute(), "add-imap")

	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Equal("IMAP account added successfully!\n", stdout.String(), "stdout")
	assert.Contains(stderr.String(), "Using password from MSGVAULT_IMAP_PASSWORD", "stderr")
}
