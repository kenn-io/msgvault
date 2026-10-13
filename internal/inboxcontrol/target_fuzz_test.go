package inboxcontrol

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Untrusted native identifiers containing a control character cannot bind an
// action, regardless of the surrounding bytes or which identity field it is in.
func FuzzTargetRejectsControlIdentity(f *testing.F) {
	f.Add("native-message", uint8(0))
	f.Add("", uint8(1))
	f.Add("\xff", uint8(2))
	f.Add("日本語", uint8(0))
	f.Fuzz(func(t *testing.T, surrounding string, field uint8) {
		target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test", Scope: ScopeMessage, ItemID: 2, ProviderID: "native-message"}
		untrusted := surrounding + "\n"
		switch field % 3 {
		case 0:
			target.ProviderID = untrusted
		case 1:
			target.SourceIdentifier = untrusted
		case 2:
			target.AccountID = untrusted
		}
		assert.ErrorIs(t, target.Validate(), ErrInvalid)
	})
}
