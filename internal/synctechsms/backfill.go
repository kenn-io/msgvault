package synctechsms

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// BackfillNativeSendStateContext repairs one bounded batch of messages imported
// before the native send-state column was populated. The deriver can continue
// processing queued conversations after each batch; later passes resume from
// the durable cursor.
func BackfillNativeSendStateContext(ctx context.Context, st *store.Store) (int, error) {
	progress, err := backfillNativeSendStateBatchContext(ctx, st, store.SynctechSMSNativeStateBackfillBatchSize)
	return progress.Unsent, err
}

func backfillNativeSendStateBatchContext(ctx context.Context, st *store.Store, batchSize int) (store.SynctechSMSNativeStateBackfillProgress, error) {
	return st.ProcessSynctechSMSNativeStateBackfillBatchContext(ctx, batchSize, func(candidate store.SynctechSMSStateBackfillCandidate) (bool, error) {
		return inspectNativeSendState(candidate.MessageType, candidate.CompressedRaw, candidate.Compression.String)
	})
}

func inspectNativeSendState(messageType string, compressed []byte, compression string) (sent bool, err error) {
	var reader io.Reader = bytes.NewReader(compressed)
	var closer io.Closer
	if compression != "" {
		if compression != "zlib" {
			return false, fmt.Errorf("unsupported raw compression %q", compression)
		}
		zlibReader, err := zlib.NewReader(reader)
		if err != nil {
			return false, fmt.Errorf("open compressed raw record: %w", err)
		}
		reader = zlibReader
		closer = zlibReader
	}
	if closer != nil {
		defer func() {
			if closeErr := closer.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close compressed raw record: %w", closeErr)
			}
		}()
	}

	field := ""
	switch messageType {
	case "sms":
		field = "Type"
	case "mms":
		field = "MessageBox"
	default:
		return false, fmt.Errorf("unsupported stored Synctech message type %q", messageType)
	}
	state, err := readNativeSendState(jsontext.NewDecoder(reader), field)
	if err != nil {
		return false, err
	}
	if messageType == "sms" {
		stateValue := nativeSentState(SMS{Type: SMSType(state)})
		if !stateValue.Valid {
			return false, fmt.Errorf("SMS type %d has no native send state", state)
		}
		return stateValue.Bool, nil
	}
	stateValue := nativeSentState(MMS{MessageBox: MMSBox(state)})
	if !stateValue.Valid {
		return false, fmt.Errorf("MMS box %d has no native send state", state)
	}
	return stateValue.Bool, nil
}

func readNativeSendState(decoder *jsontext.Decoder, field string) (int64, error) {
	start, err := decoder.ReadToken()
	if err != nil {
		return 0, fmt.Errorf("read raw record object: %w", err)
	}
	if start.Kind() != '{' {
		return 0, fmt.Errorf("raw record must be a JSON object")
	}
	for {
		key, err := decoder.ReadToken()
		if err != nil {
			return 0, fmt.Errorf("read raw record field: %w", err)
		}
		if key.Kind() == '}' {
			return 0, fmt.Errorf("raw record has no %s field", field)
		}
		if key.Kind() != '"' {
			return 0, fmt.Errorf("raw record field name is not a string")
		}
		name := key.String()
		if strings.EqualFold(name, field) {
			value, err := decoder.ReadToken()
			if err != nil {
				return 0, fmt.Errorf("read native send-state field %s: %w", field, err)
			}
			if value.Kind() != '0' {
				return 0, fmt.Errorf("native send-state field %s is not an integer", field)
			}
			state, err := strconv.ParseInt(value.String(), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse native send-state field %s: %w", field, err)
			}
			return state, nil
		}
		if err := decoder.SkipValue(); err != nil {
			return 0, fmt.Errorf("skip raw record field %s: %w", name, err)
		}
	}
}
