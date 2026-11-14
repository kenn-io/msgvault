package export

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

// MediaSlots tracks the original attachment slots when an import is replayed.
type MediaSlots struct {
	Existing   map[string]store.AttachmentRef
	newMessage bool
}

func LoadMediaSlots(ctx context.Context, st *store.Store, messageID int64) (*MediaSlots, error) {
	slots := &MediaSlots{Existing: map[string]store.AttachmentRef{}, newMessage: messageID == 0}
	if messageID == 0 {
		return slots, nil
	}
	refs, err := st.MessageMIMEAttachmentsContext(ctx, messageID)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		slots.Existing[ref.SourcePartKey] = ref
	}
	return slots, nil
}

func (s *MediaSlots) Pending(key string) bool {
	ref, ok := s.Existing[key]
	return s.newMessage || (ok && ref.ContentHash == "")
}

func (s *MediaSlots) Keep(write store.AttachmentWrite) bool {
	return s.newMessage || write.ContentHash != ""
}
