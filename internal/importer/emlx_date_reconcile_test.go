package importer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportEmlxReconcilePreservesArchivedPlistDate(t *testing.T) {
	plistDate := time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		header     string
		laterPlist string
		wantSent   time.Time
	}{
		{name: "missing plist", wantSent: plistDate},
		{
			name:       "invalid plist date",
			laterPlist: `<plist><dict><key>date-sent</key><real>invalid</real></dict></plist>`,
			wantSent:   plistDate,
		},
		{
			name:     "MIME date remains distinct from plist date",
			header:   "Date: Fri, 02 Jan 2009 03:04:05 +0000\r\n",
			wantSent: time.Date(2009, 1, 2, 3, 4, 5, 0, time.UTC),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Inbox.mbox")
			raw := []byte("From: sender@example.test\r\n" +
				"To: owner@example.test\r\n" +
				"Subject: Synthetic date fixture\r\n" + tc.header +
				"\r\nMessage with a source metadata date.\r\n")
			mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
			const firstPlist = `<plist><dict><key>date-sent</key><integer>252460800</integer></dict></plist>`
			r.NoError(os.WriteFile(filepath.Join(root, "Messages", "1.emlx"),
				fmt.Appendf(nil, "%d\n%s%s", len(raw), raw, firstPlist), 0600))
			opts := EmlxImportOptions{Identifier: "owner@example.test"}
			first, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			r.False(first.HardErrors)
			var messageID int64
			var sent, internal sql.NullTime
			r.NoError(st.DB().QueryRow("SELECT id, sent_at, internal_date FROM messages").Scan(&messageID, &sent, &internal))
			r.Equal(sql.NullTime{Time: tc.wantSent, Valid: true}, sent)
			r.Equal(sql.NullTime{Time: plistDate, Valid: true}, internal)

			// Only the copy without a usable plist date remains, so reconciliation
			// must ingest it and fall back to the archived date.
			r.NoError(os.Remove(filepath.Join(root, "Messages", "1.emlx")))
			r.NoError(os.WriteFile(filepath.Join(root, "Messages", "2.emlx"),
				fmt.Appendf(nil, "%d\n%s%s", len(raw), raw, tc.laterPlist), 0600))
			opts.FullReconcile = true
			reconciled, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			r.False(reconciled.HardErrors)
			a.Zero(reconciled.MessagesAdded, "identical MIME must retain the shared archived message")
			a.Equal(int64(1), reconciled.MessagesUpdated, "reconciliation ingests the remaining copy")
			r.NoError(st.DB().QueryRow("SELECT sent_at, internal_date FROM messages WHERE id = ?", messageID).Scan(&sent, &internal))
			a.Equal(sql.NullTime{Time: tc.wantSent, Valid: true}, sent)
			a.Equal(sql.NullTime{Time: plistDate, Valid: true}, internal)
		})
	}
}
