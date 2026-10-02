package cmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestServeFlagsOverrideMalformedEnvironment(t *testing.T) { //nolint:paralleltest // process environment and logger
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_BIND_ADDR", "")
	t.Setenv("MSGVAULT_API_PORT", "invalid")
	root := newRootCommand()
	var got *config.Config
	leaf := &cobra.Command{Use: "serve", RunE: func(cmd *cobra.Command, _ []string) error {
		got = invocationFromCommand(cmd).cfg
		return nil
	}}
	addServeConfigFlags(leaf)
	root.AddCommand(leaf)
	root.SetArgs([]string{"--home", t.TempDir(), "serve", "--bind", "127.0.0.1", "--port", "8181"})
	require.NoError(root.Execute())
	require.NotNil(got)
	assert.Equal("127.0.0.1", got.Server.BindAddr)
	assert.Equal(8181, got.Server.APIPort)
	if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
		inv.logResult.Close()
	}
}

func TestServeResolvesRealLoopbackInterface(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	interfaces, err := net.Interfaces()
	require.NoError(err)
	var name string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			name = iface.Name
			break
		}
	}
	if name == "" {
		t.Skip("no active loopback interface")
	}
	bind, err := resolveServeBind("iface:" + name)
	require.NoError(err)
	require.NotNil(net.ParseIP(bind))
	assert.True(net.ParseIP(bind).IsLoopback())
	listener, err := listenServeAPI("iface:"+name, 0)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(listener.Close()) })
	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(ok, "listener address should be TCP")
	assert.True(addr.IP.IsLoopback())
}

func TestServeSavePreservesInterfaceSelector(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	interfaces, err := net.Interfaces()
	require.NoError(err)
	var name string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			name = iface.Name
			break
		}
	}
	if name == "" {
		t.Skip("no active loopback interface")
	}

	selector := "iface:" + name
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Server.BindAddr = selector
	require.NoError(prepareServeConfig(cfg))
	assert.NotEqual(selector, cfg.Server.BindAddr, "runtime bind should be resolved")
	cfg.Accounts = []config.AccountSchedule{{Email: "person@example.com", Enabled: true}}
	require.NoError(cfg.Save())

	reloaded, err := config.Load("", cfg.HomeDir)
	require.NoError(err)
	assert.Equal(selector, reloaded.Server.BindAddr, "saved config should retain the interface selector")
}

func TestServeUnknownInterfaceFailsBeforeMinting(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Server.BindAddr = "iface:msgvault-nonexistent-test-interface"
	require.Error(t, prepareServeConfig(cfg))
	_, err := os.Stat(cfg.ServerKeyFilePath())
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestServeEnvironmentOnlyMintsAndReusesKey(t *testing.T) { //nolint:paralleltest // process environment
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	t.Setenv("MSGVAULT_BIND_ADDR", "0.0.0.0")
	cfg, err := config.Load("", home)
	require.NoError(err)
	clientConfig, err := config.Load("", home)
	require.NoError(err)
	require.NoError(clientConfig.ResolveServerKey())
	assert.Empty(clientConfig.Server.AuthenticationKey())
	require.NoError(prepareServeConfig(cfg))
	key := cfg.Server.AuthenticationKey()
	assert.Len(key, 43)
	assert.Empty(cfg.Server.APIKey, "runtime keys stay out of TOML")
	require.NoError(cfg.Server.ValidateSecure())
	restarted, err := config.Load("", home)
	require.NoError(err)
	require.NoError(prepareServeConfig(restarted))
	assert.Equal(key, restarted.Server.AuthenticationKey())
	_, err = os.Stat(filepath.Join(home, "config.toml"))
	require.ErrorIs(err, os.ErrNotExist)
	// An already-loaded client sees a key minted by its child daemon.
	rt := &DaemonRuntime{}
	rt.Record.Metadata = map[string]string{runtimeAuthFingerprint: daemonAPIKeyFingerprint(key)}
	require.NoError(localDaemonAuthIdentityError("http://127.0.0.1:8080", rt, clientConfig))
	assert.Equal(key, clientConfig.Server.AuthenticationKey())
}

func TestServeExplicitSecretFailureDoesNotMint(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Server.BindAddr = "0.0.0.0"
	cfg.Server.APIKeyFile = filepath.Join(cfg.HomeDir, "missing-key")
	require.Error(t, prepareServeConfig(cfg))
	_, err := os.Stat(cfg.ServerKeyFilePath())
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestServeExplicitInsecureDoesNotReadOrMintDefaultKey(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Server.BindAddr = "0.0.0.0"
	cfg.Server.AllowInsecure = true
	require.NoError(os.MkdirAll(filepath.Dir(cfg.ServerKeyFilePath()), 0o700))
	require.NoError(os.WriteFile(cfg.ServerKeyFilePath(), nil, 0o600))
	require.NoError(prepareServeConfig(cfg))
	assert.Empty(cfg.Server.AuthenticationKey())
	data, err := os.ReadFile(cfg.ServerKeyFilePath())
	require.NoError(err)
	assert.Empty(data)
}
