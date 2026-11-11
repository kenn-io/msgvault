package beeperidentity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProviderKeyExtractionPreservesBeeperBytes(t *testing.T) {
	for _, tc := range []struct{ service, kind, scope, native, want string }{
		{"signal", "account", "signal", "example-id", "provider:6:signal:7:account:6:signal:10:example-id"},
		{"x", "service", "", "1001", "provider:1:x:7:service:0::4:1001"},
		{"a:b", "c", "", "d:e", "provider:3:a:b:1:c:0::3:d:e"},
		{"", "", "", "", "provider:0::0::0::0:"},
		{"é", "account", "a:b", " id ", "provider:2:é:7:account:3:a:b:4: id "},
	} {
		assert.Equal(t, tc.want, ProviderIdentityKey(tc.service, tc.kind, tc.scope, tc.native))
	}
}
