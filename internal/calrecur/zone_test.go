package calrecur

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadZone(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"Europe/Berlin", "Europe/Berlin"},
		{"Eastern Standard Time", "America/New_York"},
		{"tzone://Microsoft/Eastern Standard Time", "America/New_York"},
		{"tzone://Microsoft/America/New_York", "America/New_York"},
		{"/vendor:1/America/New_York", "America/New_York"},
		{"/freeassociation.sourceforge.net/Europe/Berlin", "Europe/Berlin"},
		{"/example.org/America/Argentina/Buenos_Aires", "America/Argentina/Buenos_Aires"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zone, err := LoadZone(tc.name)
			require.NoError(t, err)
			assert.Equal(t, tc.want, zone.String())
		})
	}
	_, err := LoadZone("/example.org/Invalid/Zone")
	require.Error(t, err)
}
