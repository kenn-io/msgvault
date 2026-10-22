package storetest

import (
	"context"
	"iter"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// LoopOriginalReadStore supplies one open question followed by outbound
// messages whose stored originals may change the detected answer.
type LoopOriginalReadStore struct {
	*store.Store

	Conversations []store.LoopConversation
	OriginalReads int
}

func loopToRecipient(participant store.LoopParticipant) store.LoopRecipient {
	return store.LoopRecipient{LoopParticipant: participant, Type: "to"}
}

// NewLoopOriginalReadStore creates a question followed by the requested number
// of outbound acknowledgement conversations.
func NewLoopOriginalReadStore(st *store.Store, acknowledgements int, now time.Time) *LoopOriginalReadStore {
	questionAt := now.Add(-11 * 24 * time.Hour)
	conversations := []store.LoopConversation{{
		ConversationID: 1, ConversationType: "email_thread", FirstAt: questionAt,
		Messages: []store.LoopMessage{{
			ID: 1, ConversationID: 1, OccurredAt: questionAt, Direction: store.DirectionInbound,
			Snippet:    "Can you send the contract?",
			Sender:     store.LoopParticipant{ParticipantID: 2, DisplayName: "Alex Example", Address: "alex@example.test"},
			Recipients: []store.LoopRecipient{loopToRecipient(store.LoopParticipant{ParticipantID: 1, IsOwner: true})},
		}},
	}}
	for i := range acknowledgements {
		id := int64(i + 2)
		at := now.Add(-10*24*time.Hour + time.Duration(i)*time.Second)
		conversations = append(conversations, store.LoopConversation{
			ConversationID: id, ConversationType: "email_thread", FirstAt: at,
			Messages: []store.LoopMessage{{
				ID: id, ConversationID: id, OccurredAt: at, Direction: store.DirectionOutbound,
				Sender:                store.LoopParticipant{ParticipantID: 1, DisplayName: "Me", Address: OwnerAddress, IsOwner: true},
				Recipients:            []store.LoopRecipient{loopToRecipient(store.LoopParticipant{ParticipantID: id + 1, Address: "recipient@example.test"})},
				MayHaveFileAttachment: true,
			}},
		})
	}
	return &LoopOriginalReadStore{Store: st, Conversations: conversations}
}

func (s *LoopOriginalReadStore) LoopConversationsContext(_ context.Context, _ store.LoopQuery) iter.Seq2[store.LoopConversation, error] {
	return func(yield func(store.LoopConversation, error) bool) {
		for _, conversation := range s.Conversations {
			if !yield(conversation, nil) {
				return
			}
		}
	}
}

func (s *LoopOriginalReadStore) GetMessageBodyTextContext(_ context.Context, messageID int64) (string, error) {
	if messageID == 1 {
		return "Can you send the contract?", nil
	}
	return "Thanks!", nil
}

func (s *LoopOriginalReadStore) LoopOriginalEvidenceContext(ctx context.Context, _ int64) (mime.AutomationSignals, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return mime.AutomationSignals{}, false, false, err
	}
	s.OriginalReads++
	return mime.AutomationSignals{}, false, false, nil
}
