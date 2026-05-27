package applemail

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// createTestAccountsDB creates a temporary Accounts4.sqlite with the
// minimal schema and populates it with the given accounts.
func createTestAccountsDB(t *testing.T, accounts []testAccount) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "Accounts4.sqlite")
	db, err := sql.Open("sqlite3", dbPath)
	requirepkg.NoError(t, err, "create test db")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE ZACCOUNT (
			Z_PK INTEGER PRIMARY KEY,
			ZIDENTIFIER TEXT,
			ZUSERNAME TEXT,
			ZACCOUNTDESCRIPTION TEXT,
			ZPARENTACCOUNT INTEGER
		)
	`)
	requirepkg.NoError(t, err, "create schema")

	for _, a := range accounts {
		_, err := db.Exec(
			`INSERT INTO ZACCOUNT (Z_PK, ZIDENTIFIER, ZUSERNAME, ZACCOUNTDESCRIPTION, ZPARENTACCOUNT)
			 VALUES (?, ?, ?, ?, ?)`,
			a.pk, a.identifier, a.username, a.description, a.parentAccount,
		)
		requirepkg.NoError(t, err, "insert account")
	}

	return dbPath
}

type testAccount struct {
	pk            int
	identifier    string
	username      *string
	description   *string
	parentAccount *int
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	requirepkg.NoError(t, os.MkdirAll(path, 0o755), "mkdir %q", path)
}

func TestResolveAccounts(t *testing.T) {
	// Set up accounts mimicking real Accounts4.sqlite:
	// - PK 1: Google parent (has email, description "Google")
	// - PK 2: IMAP child of Google (GUID, no email, inherits from parent)
	// - PK 3: Yahoo parent (has email, description "Yahoo!")
	// - PK 4: IMAP child of Yahoo (GUID, no email, inherits from parent)
	// - PK 5: Exchange account (GUID, has own email)
	// - PK 6: "On My Mac" (GUID, no email, description only)
	// - PK 7: iCloud parent (has email, description "iCloud")
	// - PK 8: IMAP child of iCloud with empty-string fields (not NULL)
	accounts := []testAccount{
		{pk: 1, identifier: "google-parent-id", username: new("user@gmail.com"), description: new("Google"), parentAccount: nil},
		{pk: 2, identifier: "13C9A646-1234-5678-9ABC-E07FFBDDEED3", username: nil, description: nil, parentAccount: new(1)},
		{pk: 3, identifier: "yahoo-parent-id", username: new("user@yahoo.com"), description: new("Yahoo!"), parentAccount: nil},
		{pk: 4, identifier: "AABBCCDD-1111-2222-3333-445566778899", username: nil, description: nil, parentAccount: new(3)},
		{pk: 5, identifier: "EXCHANGE1-AAAA-BBBB-CCCC-DDDDEEEEEEEE", username: new("user@exchange.com"), description: new("Exchange"), parentAccount: nil},
		{pk: 6, identifier: "LOCALONLY-0000-0000-0000-000000000000", username: nil, description: new("On My Mac"), parentAccount: nil},
		{pk: 7, identifier: "icloud-parent-id", username: new("user@icloud.com"), description: new("iCloud"), parentAccount: nil},
		{pk: 8, identifier: "ICLOUDCH-1111-2222-3333-444455556666", username: new(""), description: new(""), parentAccount: new(7)},
	}

	dbPath := createTestAccountsDB(t, accounts)

	tests := []struct {
		name        string
		guids       []string
		wantLen     int
		wantEmail   map[string]string // guid → expected email
		wantDesc    map[string]string // guid → expected description
		wantMissing []string          // guids not in result
	}{
		{
			name:    "IMAP child resolves parent email (Google)",
			guids:   []string{"13C9A646-1234-5678-9ABC-E07FFBDDEED3"},
			wantLen: 1,
			wantEmail: map[string]string{
				"13C9A646-1234-5678-9ABC-E07FFBDDEED3": "user@gmail.com",
			},
			wantDesc: map[string]string{
				"13C9A646-1234-5678-9ABC-E07FFBDDEED3": "Google",
			},
		},
		{
			name:    "IMAP child resolves parent email (Yahoo)",
			guids:   []string{"AABBCCDD-1111-2222-3333-445566778899"},
			wantLen: 1,
			wantEmail: map[string]string{
				"AABBCCDD-1111-2222-3333-445566778899": "user@yahoo.com",
			},
			wantDesc: map[string]string{
				"AABBCCDD-1111-2222-3333-445566778899": "Yahoo!",
			},
		},
		{
			name:    "Exchange account with own email",
			guids:   []string{"EXCHANGE1-AAAA-BBBB-CCCC-DDDDEEEEEEEE"},
			wantLen: 1,
			wantEmail: map[string]string{
				"EXCHANGE1-AAAA-BBBB-CCCC-DDDDEEEEEEEE": "user@exchange.com",
			},
			wantDesc: map[string]string{
				"EXCHANGE1-AAAA-BBBB-CCCC-DDDDEEEEEEEE": "Exchange",
			},
		},
		{
			name:    "On My Mac has no email",
			guids:   []string{"LOCALONLY-0000-0000-0000-000000000000"},
			wantLen: 1,
			wantEmail: map[string]string{
				"LOCALONLY-0000-0000-0000-000000000000": "",
			},
			wantDesc: map[string]string{
				"LOCALONLY-0000-0000-0000-000000000000": "On My Mac",
			},
		},
		{
			name:        "Missing GUID returns no entry",
			guids:       []string{"NOTEXIST-0000-0000-0000-000000000000"},
			wantLen:     0,
			wantMissing: []string{"NOTEXIST-0000-0000-0000-000000000000"},
		},
		{
			name:    "Multiple GUIDs resolved at once",
			guids:   []string{"13C9A646-1234-5678-9ABC-E07FFBDDEED3", "AABBCCDD-1111-2222-3333-445566778899"},
			wantLen: 2,
			wantEmail: map[string]string{
				"13C9A646-1234-5678-9ABC-E07FFBDDEED3": "user@gmail.com",
				"AABBCCDD-1111-2222-3333-445566778899": "user@yahoo.com",
			},
		},
		{
			name:    "Empty-string child fields fall through to parent",
			guids:   []string{"ICLOUDCH-1111-2222-3333-444455556666"},
			wantLen: 1,
			wantEmail: map[string]string{
				"ICLOUDCH-1111-2222-3333-444455556666": "user@icloud.com",
			},
			wantDesc: map[string]string{
				"ICLOUDCH-1111-2222-3333-444455556666": "iCloud",
			},
		},
		{
			name:    "Empty GUID list",
			guids:   nil,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			result, err := ResolveAccounts(dbPath, tt.guids)
			requirepkg.NoError(t, err, "ResolveAccounts")

			assert.Len(result, tt.wantLen)

			for guid, wantEmail := range tt.wantEmail {
				info, ok := result[guid]
				if !assert.True(ok, "GUID %s not found in result", guid) {
					continue
				}
				assert.Equal(wantEmail, info.Email, "GUID %s email", guid)
			}

			for guid, wantDesc := range tt.wantDesc {
				info, ok := result[guid]
				if !ok {
					continue // already reported above
				}
				assert.Equal(wantDesc, info.Description, "GUID %s description", guid)
			}

			for _, guid := range tt.wantMissing {
				_, ok := result[guid]
				assert.False(ok, "GUID %s should not be in result", guid)
			}
		})
	}
}

func TestResolveAccounts_BadPath(t *testing.T) {
	_, err := ResolveAccounts("/nonexistent/path/Accounts4.sqlite", []string{"some-guid"})
	requirepkg.Error(t, err, "expected error for bad DB path")
}

func TestAccountInfo_Identifier(t *testing.T) {
	tests := []struct {
		name string
		info AccountInfo
		want string
	}{
		{
			name: "has email",
			info: AccountInfo{Email: "user@gmail.com", Description: "Google"},
			want: "user@gmail.com",
		},
		{
			name: "no email uses description",
			info: AccountInfo{Email: "", Description: "On My Mac"},
			want: "On My Mac",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertpkg.Equal(t, tt.want, tt.info.Identifier())
		})
	}
}

func TestDiscoverV10Accounts(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Create a fake Mail directory with V10 layout.
	mailDir := t.TempDir()
	v10Dir := filepath.Join(mailDir, "V10")
	guid1 := "13C9A646-1234-5678-9ABC-E07FFBDDEED3"
	guid2 := "AABBCCDD-1111-2222-3333-445566778899"

	// Create UUID dirs under V10.
	require.NoError(os.MkdirAll(filepath.Join(v10Dir, guid1), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(v10Dir, guid2), 0o755))
	// Also create a non-UUID dir that should be ignored.
	require.NoError(os.MkdirAll(filepath.Join(v10Dir, "MailData"), 0o755))

	// Create accounts DB with these GUIDs.
	accounts := []testAccount{
		{pk: 1, identifier: "google-parent", username: new("user@gmail.com"), description: new("Google"), parentAccount: nil},
		{pk: 2, identifier: guid1, username: nil, description: nil, parentAccount: new(1)},
		{pk: 3, identifier: "yahoo-parent", username: new("user@yahoo.com"), description: new("Yahoo!"), parentAccount: nil},
		{pk: 4, identifier: guid2, username: nil, description: nil, parentAccount: new(3)},
	}
	dbPath := createTestAccountsDB(t, accounts)

	result, err := DiscoverV10Accounts(mailDir, dbPath, nil)
	require.NoError(err, "DiscoverV10Accounts")

	require.Len(result, 2)

	// Check both accounts resolved.
	byGUID := make(map[string]AccountInfo)
	for _, a := range result {
		byGUID[a.GUID] = a
	}

	if info, ok := byGUID[guid1]; !assert.True(ok, "GUID %s not found", guid1) {
		// nothing
	} else {
		assert.Equal("user@gmail.com", info.Email, "GUID %s email", guid1)
	}

	if info, ok := byGUID[guid2]; !assert.True(ok, "GUID %s not found", guid2) {
		// nothing
	} else {
		assert.Equal("user@yahoo.com", info.Email, "GUID %s email", guid2)
	}
}

func TestFindV10GUIDs(t *testing.T) {
	guidA := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	guidB := "11111111-2222-3333-4444-555555555555"
	guidC := "99999999-8888-7777-6666-555544443333"

	tests := []struct {
		name      string
		setup     func(t *testing.T, mailDir string)
		wantGUIDs []string
	}{
		{
			name: "single V10 dir",
			setup: func(t *testing.T, mailDir string) {
				t.Helper()
				mustMkdirAll(t, filepath.Join(mailDir, "V10", guidA))
				mustMkdirAll(t, filepath.Join(mailDir, "V10", "MailData"))
			},
			wantGUIDs: []string{guidA},
		},
		{
			name: "same GUID in V2 and V10 deduplicates",
			setup: func(t *testing.T, mailDir string) {
				t.Helper()
				mustMkdirAll(t, filepath.Join(mailDir, "V2", guidA))
				mustMkdirAll(t, filepath.Join(mailDir, "V10", guidA))
			},
			wantGUIDs: []string{guidA},
		},
		{
			name: "partially populated V10 discovers older-only accounts",
			setup: func(t *testing.T, mailDir string) {
				t.Helper()
				mustMkdirAll(t, filepath.Join(mailDir, "V10", guidA))
				mustMkdirAll(t, filepath.Join(mailDir, "V9", guidA))
				mustMkdirAll(t, filepath.Join(mailDir, "V9", guidB))
			},
			wantGUIDs: []string{guidA, guidB},
		},
		{
			name: "empty V10 discovers from V9",
			setup: func(t *testing.T, mailDir string) {
				t.Helper()
				mustMkdirAll(t, filepath.Join(mailDir, "V10"))
				mustMkdirAll(t, filepath.Join(mailDir, "V9", guidC))
			},
			wantGUIDs: []string{guidC},
		},
		{
			name: "non-V directory ignored",
			setup: func(t *testing.T, mailDir string) {
				t.Helper()
				mustMkdirAll(t, filepath.Join(mailDir, "Other", guidA))
			},
			wantGUIDs: nil,
		},
		{
			name: "non-UUID subdirs ignored",
			setup: func(t *testing.T, mailDir string) {
				t.Helper()
				mustMkdirAll(t, filepath.Join(mailDir, "V10", "MailData"))
			},
			wantGUIDs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mailDir := t.TempDir()
			tt.setup(t, mailDir)

			guids, err := findV10GUIDs(mailDir)
			requirepkg.NoError(t, err, "findV10GUIDs")

			assertpkg.ElementsMatch(t, tt.wantGUIDs, guids)
		})
	}
}

// writeTestEmlx creates a minimal .emlx file at the given path.
func writeTestEmlx(t *testing.T, dir, name string) {
	t.Helper()
	mustMkdirAll(t, dir)
	path := filepath.Join(dir, name)
	requirepkg.NoError(t, os.WriteFile(path, []byte("10\nFrom: x\r\n\r\n"), 0o600), "write %q", path)
}

func TestV10AccountDir_PrefersPopulated(t *testing.T) {
	guid := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	mailDir := t.TempDir()

	// V10 has the GUID dir with an empty .mbox stub (no .emlx files).
	mustMkdirAll(t, filepath.Join(mailDir, "V10", guid, "INBOX.mbox", "Messages"))

	// V9 has actual messages.
	writeTestEmlx(t,
		filepath.Join(mailDir, "V9", guid, "INBOX.mbox", "Messages"),
		"1.emlx",
	)

	got, err := V10AccountDir(mailDir, guid)
	requirepkg.NoError(t, err, "V10AccountDir")

	want := filepath.Join(mailDir, "V9", guid)
	assertpkg.Equal(t, want, got, "should prefer populated V9 over empty V10")
}

func TestV10AccountDir_NewestPopulatedWins(t *testing.T) {
	guid := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	mailDir := t.TempDir()

	// Both have actual messages; newest wins.
	writeTestEmlx(t,
		filepath.Join(mailDir, "V10", guid, "INBOX.mbox", "Messages"),
		"1.emlx",
	)
	writeTestEmlx(t,
		filepath.Join(mailDir, "V9", guid, "INBOX.mbox", "Messages"),
		"1.emlx",
	)

	got, err := V10AccountDir(mailDir, guid)
	requirepkg.NoError(t, err, "V10AccountDir")

	want := filepath.Join(mailDir, "V10", guid)
	assertpkg.Equal(t, want, got, "newest populated should win")
}
