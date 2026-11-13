package line

import (
	"errors"
	"fmt"

	"howett.net/plist"
)

// metadata holds the entries of ZMESSAGE.ZCONTENTMETADATA the importer reads.
// The column is an NSKeyedArchiver dictionary of strings; entries not listed
// here (mentions, media descriptors, sticker ids) are ignored.
type metadata struct {
	ReplyTo    string // replyOriginalMessageId: the LINE message id this message answers
	SystemType string // systemMessageTypeName: set on notices LINE renders as system text
	AltText    string // ALT_TEXT: the readable text of a rich message
}

// decodeMetadata reads the entries it knows from a keyed archive.
func decodeMetadata(blob []byte) (metadata, error) {
	var md metadata
	if len(blob) == 0 {
		return md, nil
	}
	var archive struct {
		Top     map[string]plist.UID `plist:"$top"`
		Objects []any                `plist:"$objects"`
	}
	if _, err := plist.Unmarshal(blob, &archive); err != nil {
		return md, errors.New("metadata plist is unreadable")
	}
	resolve := func(v any) (any, bool) {
		if uid, ok := v.(plist.UID); ok {
			if uid == 0 || uid >= plist.UID(len(archive.Objects)) {
				return nil, false
			}
			return archive.Objects[uid], true
		}
		return v, true
	}
	rootID, ok := archive.Top["root"]
	if !ok {
		return md, errors.New("metadata has no root")
	}
	rootValue, ok := resolve(rootID)
	root, isMap := rootValue.(map[string]any)
	if !ok || !isMap {
		return md, errors.New("metadata root is not a dictionary")
	}
	keys, keysOK := root["NS.keys"].([]any)
	values, valuesOK := root["NS.objects"].([]any)
	if !keysOK || !valuesOK || len(keys) != len(values) {
		return md, errors.New("metadata dictionary has invalid keys or values")
	}
	for i, k := range keys {
		keyValue, ok := resolve(k)
		key, isString := keyValue.(string)
		if !ok || !isString {
			return md, errors.New("metadata dictionary has an invalid key")
		}
		if key != "replyOriginalMessageId" && key != "systemMessageTypeName" && key != "ALT_TEXT" {
			continue
		}
		value, ok := resolve(values[i])
		str, isString := value.(string)
		if !ok || !isString {
			return md, fmt.Errorf("metadata field %s is not a readable string", key)
		}
		switch key {
		case "replyOriginalMessageId":
			md.ReplyTo = str
		case "systemMessageTypeName":
			md.SystemType = str
		case "ALT_TEXT":
			md.AltText = str
		}
	}
	return md, nil
}
