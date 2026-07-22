//go:build !windows

// Descriptor discovery is only supported where descriptor files can be
// verified with Unix permission and ownership semantics; on Windows
// descriptorFileSecurityCheck fails closed (see discovery_windows.go), so
// these tests exercise Unix-only behavior.

package taskclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverDescriptorSecurityAndProtocol(t *testing.T) {
	t.Run("regular descriptor", func(t *testing.T) {
		path := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-test",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)

		client, err := Discover(context.Background(), DiscoveryOptions{
			DescriptorPath: path,
			APIKey:         "configured-test-key",
		})

		require.NoError(t, err)
		assert.Equal(t, "instance-test", client.InstanceID())
		assert.Equal(t, EndpointLoopbackHTTP, client.EndpointKind())
	})

	t.Run("symlink", func(t *testing.T) {
		target := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-test",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)
		link := filepath.Join(t.TempDir(), "descriptor.json")
		require.NoError(t, os.Symlink(target, link))

		_, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: link, APIKey: "test-key"})

		assert.ErrorIs(t, err, ErrInsecureDescriptor)
	})

	t.Run("wrong owner", func(t *testing.T) {
		path := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-test",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)
		owner, err := fileOwnerID(path)
		require.NoError(t, err)

		err = validateSecureRegularFile(path, owner+1)

		assert.ErrorIs(t, err, ErrInsecureDescriptor)
	})

	t.Run("permissive mode", func(t *testing.T) {
		path := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-test",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o644)

		_, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: path, APIKey: "test-key"})

		assert.ErrorIs(t, err, ErrInsecureDescriptor)
	})

	t.Run("malformed protocol", func(t *testing.T) {
		path := writeDescriptor(t, descriptor{
			ProtocolVersion: "999",
			InstanceID:      "instance-test",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)

		_, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: path, APIKey: "test-key"})

		assert.ErrorIs(t, err, ErrIncompatible)
	})
}

func TestDiscoverLoopbackAuthentication(t *testing.T) {
	t.Run("configured key", func(t *testing.T) {
		path := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-loopback",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)

		client, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: path, APIKey: "configured-test-key"})

		require.NoError(t, err)
		assert.True(t, client.HasAuthentication())
	})

	t.Run("secure token file reference", func(t *testing.T) {
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token")
		require.NoError(t, os.WriteFile(tokenPath, []byte("descriptor-test-token\n"), 0o600))
		path := filepath.Join(dir, "descriptor.json")
		writeDescriptorAt(t, path, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-loopback",
			Endpoint:        "http://127.0.0.1:32145",
			TokenFile:       tokenPath,
		}, 0o600)

		client, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: path})

		require.NoError(t, err)
		assert.True(t, client.HasAuthentication())
	})

	t.Run("missing auth", func(t *testing.T) {
		path := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-loopback",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)

		_, err := Discover(context.Background(), DiscoveryOptions{DescriptorPath: path})

		assert.ErrorIs(t, err, ErrAuthenticationRequired)
	})
}
