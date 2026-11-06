// Package emailpush provides outbound Gmail notification transport. Notifications
// are hints to reconcile an existing source, never archive data or sync cursors.
package emailpush

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/mail"
	"strconv"

	"go.kenn.io/msgvault/internal/oauth"
)

// MaxNotificationBytes bounds decoded Gmail JSON before allocation and parsing.
const MaxNotificationBytes = 4096

var (
	ErrInvalidNotification = errors.New("invalid Gmail notification")
	ErrForeignNotification = errors.New("gmail notification belongs to another account")
)

// Notification contains provider hints only. HistoryID must never be saved as
// the archive history cursor; reconciliation starts from the committed cursor.
type Notification struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    string `json:"historyId"`
}

// ParseNotification decodes Pub/Sub message data and binds it to a previously
// verified authenticated Gmail profile. Supported Gmail aliases use the same
// account normalization as the source credential code. Errors omit payloads and
// addresses so callers can log poison classifications without exposing mail.
func ParseNotification(data, verifiedEmail string) (Notification, error) {
	var zero Notification
	if len(data) == 0 || len(data) > base64.StdEncoding.EncodedLen(MaxNotificationBytes) || !bareEmail(verifiedEmail) {
		return zero, ErrInvalidNotification
	}
	var decoded []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err = encoding.Strict().DecodeString(data)
		if err == nil {
			break
		}
	}
	if err != nil || len(decoded) > MaxNotificationBytes {
		return zero, ErrInvalidNotification
	}
	var notification Notification
	if err := json.Unmarshal(decoded, &notification, json.RejectUnknownMembers(true)); err != nil || !bareEmail(notification.EmailAddress) {
		return zero, ErrInvalidNotification
	}
	id, err := strconv.ParseUint(notification.HistoryID, 10, 64)
	if err != nil || id == 0 {
		return zero, ErrInvalidNotification
	}
	if !oauth.SameGoogleAccount(notification.EmailAddress, verifiedEmail) {
		return zero, ErrForeignNotification
	}
	return notification, nil
}

func bareEmail(value string) bool {
	address, err := mail.ParseAddress(value)
	return err == nil && address.Name == "" && address.Address == value
}
