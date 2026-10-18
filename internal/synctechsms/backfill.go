package synctechsms

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// BackfillNativeSendStateContext repairs messages imported before the native
// send-state column was populated. The archived Synctech record remains the
// source of truth; changed rows enqueue their conversations through Store's
// message invalidation trigger.
func BackfillNativeSendStateContext(ctx context.Context, st *store.Store) (int, error) {
	needed, err := st.SynctechSMSNativeStateBackfillNeededContext(ctx)
	if err != nil || !needed {
		return 0, err
	}
	candidates, err := st.SynctechSMSNativeStateBackfillCandidatesContext(ctx)
	if err != nil {
		return 0, err
	}
	var unsent []int64
	for _, candidate := range candidates {
		var state struct {
			Sent  bool
			Valid bool
		}
		switch candidate.MessageType {
		case "sms":
			var record SMS
			if err := json.Unmarshal(candidate.Raw, &record); err != nil {
				return 0, fmt.Errorf("decode stored SMS state for message %d: %w", candidate.MessageID, err)
			}
			native := nativeSentState(record)
			state.Sent, state.Valid = native.Bool, native.Valid
		case "mms":
			var record MMS
			if err := json.Unmarshal(candidate.Raw, &record); err != nil {
				return 0, fmt.Errorf("decode stored MMS state for message %d: %w", candidate.MessageID, err)
			}
			native := nativeSentState(record)
			state.Sent, state.Valid = native.Bool, native.Valid
		default:
			return 0, fmt.Errorf("unsupported stored Synctech message type %q for message %d", candidate.MessageType, candidate.MessageID)
		}
		if !state.Valid {
			return 0, fmt.Errorf("stored Synctech message %d has no native send state", candidate.MessageID)
		}
		if !state.Sent {
			unsent = append(unsent, candidate.MessageID)
		}
	}
	if err := st.FinishSynctechSMSNativeStateBackfill(ctx, unsent); err != nil {
		return 0, err
	}
	return len(unsent), nil
}
