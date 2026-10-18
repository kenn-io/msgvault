package synctechsms

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

const synctechSMSNativeStateBackfillBatchSize = 100

// BackfillNativeSendStateContext repairs messages imported before the native
// send-state column was populated. Each call processes at most one batch so a
// large archive does not hold up other loop work. The archived Synctech record
// remains the source of truth; changed rows enqueue their conversations and
// progress commits together, so the next pass resumes after this batch.
func BackfillNativeSendStateContext(ctx context.Context, st *store.Store) (int, error) {
	afterMessageID, complete, err := st.SynctechSMSNativeStateBackfillProgressContext(ctx)
	if err != nil || complete {
		return 0, err
	}
	unsent, lastMessageID, processed, err := st.SynctechSMSNativeStateBackfillBatchContext(
		ctx, afterMessageID, synctechSMSNativeStateBackfillBatchSize, nativeSendStateFromStoredRecord,
	)
	if err != nil {
		return 0, err
	}
	if processed == 0 {
		lastMessageID = afterMessageID
	}
	batchComplete := processed < synctechSMSNativeStateBackfillBatchSize
	if err := st.FinishSynctechSMSNativeStateBackfillBatch(ctx, unsent, lastMessageID, batchComplete); err != nil {
		return 0, err
	}
	return len(unsent), nil
}

func nativeSendStateFromStoredRecord(messageType string, raw []byte) (sent, valid bool, err error) {
	switch messageType {
	case "sms":
		// Decode only the native state field. In particular, do not allocate MMS
		// attachment parts just to decide whether the archived message was sent.
		var record struct{ Type SMSType }
		if err := json.Unmarshal(raw, &record); err != nil {
			return false, false, err
		}
		state := nativeSentState(SMS{Type: record.Type})
		return state.Bool, state.Valid, nil
	case "mms":
		var record struct{ MessageBox MMSBox }
		if err := json.Unmarshal(raw, &record); err != nil {
			return false, false, err
		}
		state := nativeSentState(MMS{MessageBox: record.MessageBox})
		return state.Bool, state.Valid, nil
	default:
		return false, false, fmt.Errorf("unsupported message type %q", messageType)
	}
}
