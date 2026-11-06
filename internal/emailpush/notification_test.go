package emailpush

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func encodedNotification(tb testing.TB, email, history string) string {
	tb.Helper()
	data, err := json.Marshal(map[string]string{"emailAddress": email, "historyId": history})
	require.NoError(tb, err)
	return base64.StdEncoding.EncodeToString(data)
}

func TestNotificationAcceptsPositiveHistory(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Integers(uint64(1), uint64(math.MaxUint64)))
		history := strconv.FormatUint(id, 10)
		got, err := ParseNotification(encodedNotification(ht, "synthetic@example.com", history), "synthetic@example.com")
		require.NoError(ht, err)
		assert.Equal(ht, Notification{EmailAddress: "synthetic@example.com", HistoryID: history}, got)
	})
}

func TestNotificationRejectsForeignAccount(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Integers(uint64(1), uint64(math.MaxUint64)))
		suffix := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		_, err := ParseNotification(encodedNotification(ht, fmt.Sprintf("foreign%d@example.com", suffix), strconv.FormatUint(id, 10)), "synthetic@example.com")
		require.ErrorIs(ht, err, ErrForeignNotification)
		assert.NotContains(ht, err.Error(), "@example.com")
	})
}

func TestNotificationRejectsNondecimalHistory(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		hostile := hegel.Draw(ht, hegel.Text().MaxSize(80))
		_, err := ParseNotification(encodedNotification(ht, "synthetic@example.com", "x"+hostile), "synthetic@example.com")
		require.ErrorIs(ht, err, ErrInvalidNotification)
	})
}

func TestNotificationGmailAliases(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(uint64(1), uint64(math.MaxUint64)))
		alias := fmt.Sprintf("Synthetic.User+tag%d@googlemail.com", n)
		got, err := ParseNotification(encodedNotification(ht, alias, "123"), "syntheticuser@gmail.com")
		require.NoError(ht, err)
		assert.Equal(ht, alias, got.EmailAddress)
		assert.Equal(ht, "123", got.HistoryID)
	})
}

func TestNotificationRejectsMalformed(t *testing.T) {
	requirements := require.New(t)
	for _, body := range []string{
		`null`, `[]`, `{}`, `{"emailAddress":"synthetic@example.com","historyId":1}`,
		`{"emailAddress":"synthetic@example.com","historyId":"0"}`,
		`{"emailAddress":"synthetic@example.com","historyId":"-1"}`,
		`{"emailAddress":"synthetic@example.com","historyId":"+1"}`,
		`{"emailAddress":"synthetic@example.com","historyId":"1.0"}`,
		`{"emailAddress":"synthetic@example.com","historyId":"18446744073709551616"}`,
		`{"emailAddress":"synthetic@example.com","historyId":"1","historyId":"2"}`,
		`{"emailAddress":"synthetic@example.com","historyId":"1"} {}`,
		`{"emailAddress":"synthetic@example.com","historyId":"1","other":true}`,
		`{"emailAddress":"synthetic@example.com ","historyId":"1"}`,
		`{"emailAddress":"","historyId":"1"}`,
	} {
		t.Run(body, func(t *testing.T) {
			_, err := ParseNotification(base64.StdEncoding.EncodeToString([]byte(body)), "synthetic@example.com")
			require.Error(t, err)
		})
	}
	_, err := ParseNotification("!notbase64", "synthetic@example.com")
	requirements.ErrorIs(err, ErrInvalidNotification)
	_, err = ParseNotification(encodedNotification(t, "synthetic@example.com", "1"), "")
	requirements.Error(err)
	_, err = ParseNotification(strings.Repeat("A", MaxNotificationBytes*2), "synthetic@example.com")
	requirements.ErrorIs(err, ErrInvalidNotification)
	// Encoded length can pass the first check while the decoded payload exceeds its bound.
	body := strings.Repeat(" ", MaxNotificationBytes+1)
	_, err = ParseNotification(base64.StdEncoding.EncodeToString([]byte(body)), "synthetic@example.com")
	requirements.ErrorIs(err, ErrInvalidNotification)
}

func TestNotificationBase64VariantsAgree(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(uint64(1), uint64(math.MaxUint64)))
		payload, err := json.Marshal(Notification{EmailAddress: "synthetic@example.com", HistoryID: strconv.FormatUint(n, 10)})
		require.NoError(ht, err)
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			got, err := ParseNotification(encoding.EncodeToString(payload), "synthetic@example.com")
			require.NoError(ht, err)
			assert.Equal(ht, strconv.FormatUint(n, 10), got.HistoryID)
		}
	})
}

func TestNotificationPayloadBoundaryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Integers(uint64(1), uint64(math.MaxUint64)))
		want := Notification{EmailAddress: "synthetic@example.com", HistoryID: strconv.FormatUint(id, 10)}
		body, err := json.Marshal(want)
		require.NoError(ht, err)
		size := hegel.Draw(ht, hegel.Integers(len(body), MaxNotificationBytes))
		body = append(body, []byte(strings.Repeat(" ", size-len(body)))...)
		encoding := hegel.Draw(ht, hegel.SampledFrom([]*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}))
		got, err := ParseNotification(encoding.EncodeToString(body), want.EmailAddress)
		require.NoError(ht, err)
		assert.Equal(ht, want, got)
	})
}

func TestNotificationRejectsOversizedValidJSONProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		extra := hegel.Draw(ht, hegel.Integers(1, MaxNotificationBytes))
		body := []byte(`{"emailAddress":"synthetic@example.com","historyId":"1"}`)
		body = append(body, []byte(strings.Repeat(" ", MaxNotificationBytes+extra-len(body)))...)
		got, err := ParseNotification(base64.StdEncoding.EncodeToString(body), "synthetic@example.com")
		require.ErrorIs(ht, err, ErrInvalidNotification)
		assert.Zero(ht, got)
	})
}
