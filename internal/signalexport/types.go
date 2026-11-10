// Package signalexport imports the native Signal readable directory format.
package signalexport

import (
	"encoding/json/v2"
	"fmt"
	"strconv"
)

const SourceType = "signal_export"
const MessageType = "signal"

// decimal keeps proto JSON uint64s exact and distinguishes absence from zero.
type decimal string

func (d *decimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("expected proto JSON decimal string: %w", err)
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid uint64 %q: %w", s, err)
	}
	*d = decimal(strconv.FormatUint(n, 10))
	return nil
}

type backupInfo struct {
	Version         decimal `json:"version"`
	BackupTime      decimal `json:"backupTimeMs"`
	AppVersion      string  `json:"currentAppVersion"`
	FirstAppVersion string  `json:"firstAppVersion"`
}
type recipient struct {
	ID      decimal   `json:"id"`
	Self    *struct{} `json:"self"`
	Contact *struct {
		ACI          []byte `json:"aci"`
		Given        string `json:"profileGivenName"`
		Family       string `json:"profileFamilyName"`
		SystemGiven  string `json:"systemGivenName"`
		SystemFamily string `json:"systemFamilyName"`
	} `json:"contact"`
	Group *struct {
		MasterKey []byte `json:"masterKey"`
		Snapshot  struct {
			Title struct {
				Title string `json:"title"`
			} `json:"title"`
		} `json:"snapshot"`
	} `json:"group"`
}
type chat struct {
	ID          decimal `json:"id"`
	RecipientID decimal `json:"recipientId"`
}
type chatItem struct {
	ChatID        decimal          `json:"chatId"`
	AuthorID      decimal          `json:"authorId"`
	DateSent      string           `json:"dateSent"`
	Incoming      *struct{}        `json:"incoming"`
	Outgoing      *struct{}        `json:"outgoing"`
	Directionless *struct{}        `json:"directionless"`
	Pin           any              `json:"pinDetails"`
	Revisions     []chatItem       `json:"revisions"`
	Standard      *standardMessage `json:"standardMessage"`
}
type standardMessage struct {
	Text *struct {
		Body   string `json:"body"`
		Ranges []any  `json:"bodyRanges"`
	} `json:"text"`
	Attachments []struct {
		Pointer *filePointer `json:"pointer"`
		Flag    any          `json:"flag"`
	} `json:"attachments"`
	LongText    *filePointer `json:"longText"`
	Quote       any          `json:"quote"`
	Reactions   []any        `json:"reactions"`
	LinkPreview []any        `json:"linkPreview"`
}
type filePointer struct {
	ContentType string `json:"contentType"`
	FileName    string `json:"fileName"`
	Locator     struct {
		Hash []byte `json:"plaintextHash"`
	} `json:"locatorInfo"`
}
