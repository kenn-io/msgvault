package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
)

func TestAddMatrixSubprocessRejectsMissingRecoverySecretBeforeLogin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	originalHomeserver, originalUserID := addMatrixHomeserver, addMatrixUserID
	originalSkip, originalRecoveryFile := addMatrixSkipKeyBackup, addMatrixRecoveryFile
	t.Cleanup(func() {
		addMatrixHomeserver, addMatrixUserID = originalHomeserver, originalUserID
		addMatrixSkipKeyBackup, addMatrixRecoveryFile = originalSkip, originalRecoveryFile
	})
	// A direct daemon /cli/run request carries the login secret but no
	// recovery secret and does not pass --skip-key-backup.
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv(clirun.EnvMatrixLoginSecret, "synthetic-password")
	t.Setenv(clirun.EnvMatrixRecoverySecret, "")
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	cmd := newAddMatrixCmd()
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	cmd.SetArgs([]string{"--homeserver", server.URL, "--user-id", "@archive:example.org"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true

	err := cmd.Execute()

	require.ErrorContains(err, "missing Matrix recovery secret")
	assert.Zero(requests.Load(), "no Matrix device may be created without a recovery secret")
	exists, err := matrixsource.CredentialsExist(cfg.TokensDir(), "@archive:example.org")
	require.NoError(err)
	assert.False(exists)
}
