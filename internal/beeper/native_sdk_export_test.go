package beeper

import (
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Test-only access lets the SDK lifecycle use the same native HTTP fixture
// from an external test package. The daemon itself imports Beeper readers.
func NewNativeInboxSDKFixture(t *testing.T) *nativeBeeperInboxFeed {
	t.Helper()
	return newNativeBeeperInboxFeed(t)
}

func (f *nativeBeeperInboxFeed) Importer() (*Importer, ImportOptions) {
	return f.imp, f.opts
}

func (f *nativeBeeperInboxFeed) Store() *store.Store {
	return f.imp.store
}

func (f *nativeBeeperInboxFeed) AppendMessage(id string, timestamp time.Time, text, senderID string) {
	f.fake.appendMsg(f.chat.ID, fakeMsg{ID: id, SortKey: 100, Timestamp: timestamp, Text: text, SenderID: senderID})
}

func (f *nativeBeeperInboxFeed) SetReadFailure(wrongAccount bool, status int) {
	f.wrongAccount = wrongAccount
	f.detailStatus = status
}
