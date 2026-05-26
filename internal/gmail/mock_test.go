package gmail

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetupMessages_NilEntries(t *testing.T) {
	mock := NewMockAPI()

	msg1 := &RawMessage{ID: "msg1", Raw: []byte("test1")}
	msg2 := &RawMessage{ID: "msg2", Raw: []byte("test2")}

	// Should not panic when nil entries are present
	mock.SetupMessages(msg1, nil, msg2, nil)

	assert.Len(t, mock.Messages, 2, "expected 2 messages")
	assert.Same(t, msg1, mock.Messages["msg1"], "msg1 not stored correctly")
	assert.Same(t, msg2, mock.Messages["msg2"], "msg2 not stored correctly")
}

func TestSetupMessages_UninitializedMap(t *testing.T) {
	// Create mock without using constructor (simulates uninitialized map)
	mock := &MockAPI{}

	msg := &RawMessage{ID: "msg1", Raw: []byte("test")}

	// Should not panic when Messages map is nil
	mock.SetupMessages(msg)

	assert.Len(t, mock.Messages, 1, "expected 1 message")
	assert.Same(t, msg, mock.Messages["msg1"], "msg1 not stored correctly")
}

func TestSetupMessages_AllNil(t *testing.T) {
	mock := NewMockAPI()

	// Should not panic when all entries are nil
	mock.SetupMessages(nil, nil, nil)

	assert.Empty(t, mock.Messages, "expected 0 messages")
}

func TestSetupMessages_Empty(t *testing.T) {
	mock := NewMockAPI()

	// Should handle empty call gracefully
	mock.SetupMessages()

	assert.Empty(t, mock.Messages, "expected 0 messages")
}
