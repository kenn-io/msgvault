package taskclient

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverFailsClosedWithoutPlatformFileSecurity(t *testing.T) {
	path := writeDescriptor(t, descriptor{
		ProtocolVersion: ProtocolVersion,
		InstanceID:      "instance-test",
		Endpoint:        "http://127.0.0.1:32145",
	}, 0o600)

	_, err := Discover(context.Background(), DiscoveryOptions{
		DescriptorPath: path,
		APIKey:         "test-key",
		platformSecurityCheck: func() error {
			return ErrPlatformSecurityLimit
		},
	})

	require.ErrorIs(t, err, ErrPlatformSecurityLimit)
}

func TestDiscoverReportsMissingDescriptorBeforePlatformSecurityLimit(t *testing.T) {
	_, err := Discover(context.Background(), DiscoveryOptions{
		DescriptorPath: filepath.Join(t.TempDir(), "missing-descriptor.json"),
		platformSecurityCheck: func() error {
			return ErrDescriptorFileSecurityLimit
		},
	})

	require.ErrorIs(t, err, ErrNotFound)
	assert.NotErrorIs(t, err, ErrPlatformSecurityLimit)
}

func TestDiscoverUnixSocket(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are validated by the Unix test lane")
	}
	dir := t.TempDir()
	requirements.NoError(os.Chmod(dir, 0o700))
	socketPath := filepath.Join(dir, "task.sock")
	listener, err := net.Listen("unix", socketPath)
	requirements.NoError(err)
	requirements.NoError(os.Chmod(socketPath, 0o600))
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assertions.Equal("/api/v1/capabilities", r.URL.Path)
			writeTestJSON(t, w, compatibleCapabilities())
		}),
		ReadHeaderTimeout: time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		requirements.NoError(server.Close())
		assertions.ErrorIs(<-serveErr, http.ErrServerClosed)
	})
	descriptorPath := filepath.Join(dir, "descriptor.json")
	writeDescriptorAt(t, descriptorPath, descriptor{
		ProtocolVersion: ProtocolVersion,
		InstanceID:      "instance-unix",
		Endpoint:        "unix://" + socketPath,
	}, 0o600)

	client, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: descriptorPath})

	requirements.NoError(err)
	assertions.Equal(EndpointUnix, client.EndpointKind())
	capabilities, err := client.Capabilities(context.Background())
	requirements.NoError(err)
	assertions.True(capabilities.Compatible())
	if !peerCredentialsSupported() {
		assertions.Contains(client.SecurityNote(), "peer credentials unavailable")
	}
}

func TestDiscoverStaleUnixSocketIsUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket discovery is covered by Unix test lanes")
	}
	descriptorPath := writeStaleUnixDescriptor(t)

	_, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: descriptorPath})

	require.ErrorIs(t, err, ErrUnreachable)
}

func TestExplicitEndpointPolicy(t *testing.T) {
	t.Run("remote HTTP rejection", func(t *testing.T) {
		_, err := New(ClientOptions{Endpoint: "http://tasks.example.com", APIKey: "test-key"})
		assert.ErrorIs(t, err, ErrInsecureEndpoint)
	})

	t.Run("HTTPS acceptance", func(t *testing.T) {
		client, err := New(ClientOptions{Endpoint: "https://tasks.example.com", APIKey: "test-key"})
		require.NoError(t, err)
		assert.Equal(t, EndpointHTTPS, client.EndpointKind())
	})
}

func TestClientTransportBounds(t *testing.T) {
	capabilities := compatibleCapabilities()

	t.Run("redirect rejection without credential forwarding", func(t *testing.T) {
		var receivedAuthorization string
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAuthorization = r.Header.Get("Authorization")
			writeTestJSON(t, w, capabilities)
		}))
		t.Cleanup(target.Close)
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, httptest.NewRequest(http.MethodGet, target.URL, nil), target.URL, http.StatusFound)
		}))
		t.Cleanup(origin.Close)
		client := newLoopbackClient(t, origin.URL, "redirect-test-key", nil)

		_, err := client.Capabilities(context.Background())

		require.ErrorIs(t, err, ErrRedirect)
		assert.Empty(t, receivedAuthorization)
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			select {
			case <-time.After(250 * time.Millisecond):
				writeTestJSON(t, w, capabilities)
			case <-t.Context().Done():
			}
		}))
		t.Cleanup(server.Close)
		client := newLoopbackClient(t, server.URL, "timeout-test-key", func(options *ClientOptions) {
			options.Timeout = 20 * time.Millisecond
		})

		_, err := client.Capabilities(context.Background())

		assert.ErrorIs(t, err, ErrUnreachable)
	})

	t.Run("oversized body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, err := w.Write([]byte(`{"protocol_version":"1","padding":"` + strings.Repeat("x", 256) + `"}`))
			assert.NoError(t, err)
		}))
		t.Cleanup(server.Close)
		client := newLoopbackClient(t, server.URL, "body-test-key", func(options *ClientOptions) {
			options.MaxResponseBytes = 64
		})

		_, err := client.Capabilities(context.Background())

		assert.ErrorIs(t, err, ErrResponseTooLarge)
	})
}

func writeDescriptor(t *testing.T, value descriptor, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "descriptor.json")
	writeDescriptorAt(t, path, value, mode)
	return path
}

func writeStaleUnixDescriptor(t *testing.T) string {
	t.Helper()
	// t.TempDir includes the full test name and exceeds Darwin's Unix socket
	// path limit for this nested subtest, so this fixture needs a short path.
	dir, err := os.MkdirTemp("", "taskclient-stale-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	require.NoError(t, os.Chmod(dir, 0o700))
	socketPath := filepath.Join(dir, "stale.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	descriptorPath := filepath.Join(dir, "descriptor.json")
	writeDescriptorAt(t, descriptorPath, descriptor{
		ProtocolVersion: ProtocolVersion,
		InstanceID:      "instance-stale",
		Endpoint:        "unix://" + socketPath,
	}, 0o600)
	return descriptorPath
}

func writeDescriptorAt(t *testing.T, path string, value descriptor, mode os.FileMode) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, mode))
	require.NoError(t, os.Chmod(path, mode))
}

func newLoopbackClient(t *testing.T, endpoint, key string, customize func(*ClientOptions)) *Client {
	t.Helper()
	options := ClientOptions{Endpoint: endpoint, APIKey: key}
	if customize != nil {
		customize(&options)
	}
	client, err := New(options)
	require.NoError(t, err)
	return client
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(value))
}
