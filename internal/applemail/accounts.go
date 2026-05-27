package applemail

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (database/sql)
	"go.kenn.io/msgvault/internal/emlx"
)

// AccountInfo describes an Apple Mail account resolved from Accounts4.sqlite.
type AccountInfo struct {
	// GUID is the V10 directory UUID.
	GUID string

	// Email is the resolved email address. Empty for local accounts
	// like "On My Mac".
	Email string

	// Description is the account description (e.g. "Google", "Yahoo!",
	// "On My Mac").
	Description string
}

// Identifier returns the best identifier for this account: the email
// address if available, otherwise the description (e.g. "On My Mac").
func (a AccountInfo) Identifier() string {
	if a.Email != "" {
		return a.Email
	}
	return a.Description
}

// DefaultAccountsDBPath returns the default path to Apple's
// Accounts4.sqlite database.
func DefaultAccountsDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Accounts", "Accounts4.sqlite")
}

// ResolveAccounts opens the Accounts4.sqlite database at dbPath and
// resolves the given GUIDs to account information. Returns a map of
// GUID → AccountInfo for each GUID that was found.
func ResolveAccounts(dbPath string, guids []string) (map[string]AccountInfo, error) {
	if len(guids) == 0 {
		return nil, nil
	}

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open accounts db: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Build placeholders for IN clause.
	placeholders := make([]string, len(guids))
	args := make([]any, len(guids))
	for i, g := range guids {
		placeholders[i] = "?"
		args[i] = g
	}

	query := `
		SELECT
			child.ZIDENTIFIER,
			COALESCE(NULLIF(child.ZUSERNAME, ''), NULLIF(parent.ZUSERNAME, ''), '') AS email,
			COALESCE(NULLIF(parent.ZACCOUNTDESCRIPTION, ''), NULLIF(child.ZACCOUNTDESCRIPTION, ''), '') AS description
		FROM ZACCOUNT child
		LEFT JOIN ZACCOUNT parent ON parent.Z_PK = child.ZPARENTACCOUNT
		WHERE child.ZIDENTIFIER IN (` + strings.Join(placeholders, ",") + `)
	`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]AccountInfo)
	for rows.Next() {
		var guid, email, description string
		if err := rows.Scan(&guid, &email, &description); err != nil {
			return nil, fmt.Errorf("scan account row: %w", err)
		}
		result[guid] = AccountInfo{
			GUID:        guid,
			Email:       email,
			Description: description,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate account rows: %w", err)
	}

	return result, nil
}

// DiscoverV10Accounts scans mailDir for V10-style directories containing
// UUID subdirectories and resolves them to account information using the
// Accounts4.sqlite database at accountsDBPath.
func DiscoverV10Accounts(mailDir, accountsDBPath string, logger *slog.Logger) ([]AccountInfo, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Scan for V* directories containing UUID subdirectories.
	guids, err := findV10GUIDs(mailDir)
	if err != nil {
		return nil, fmt.Errorf("scan V10 directories: %w", err)
	}

	if len(guids) == 0 {
		return nil, nil
	}

	resolved, err := ResolveAccounts(accountsDBPath, guids)
	if err != nil {
		return nil, fmt.Errorf("resolve accounts: %w", err)
	}

	var accounts []AccountInfo
	for _, guid := range guids {
		info, ok := resolved[guid]
		if !ok {
			logger.Warn("GUID not found in Accounts4.sqlite, skipping",
				"guid", guid)
			continue
		}
		accounts = append(accounts, info)
	}

	return accounts, nil
}

// findV10GUIDs scans mailDir for V*/ directories containing UUID
// subdirectories and returns the unique GUIDs found. Scans all V*
// directories from newest to oldest so that partially populated
// newer directories don't hide accounts that only exist in older ones.
// Deduplicates by GUID (newest wins).
func findV10GUIDs(mailDir string) ([]string, error) {
	vDirs, err := sortedVDirs(mailDir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var guids []string
	for _, vDir := range vDirs {
		subEntries, err := os.ReadDir(vDir)
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			if sub.IsDir() && emlx.IsUUID(sub.Name()) && !seen[sub.Name()] {
				seen[sub.Name()] = true
				guids = append(guids, sub.Name())
			}
		}
	}

	return guids, nil
}

// V10AccountDir returns the path to a V10 account directory for the
// given GUID within mailDir. Searches V* directories from newest to
// oldest, preferring the newest directory that contains mailbox
// subdirectories (.mbox/.imapmbox). Falls back to the newest
// existing directory if none are populated.
func V10AccountDir(mailDir, guid string) (string, error) {
	vDirs, err := sortedVDirs(mailDir)
	if err != nil {
		return "", err
	}
	if len(vDirs) == 0 {
		return "", fmt.Errorf(
			"no V* directory found in %s", mailDir,
		)
	}

	var firstMatch string
	for _, vDir := range vDirs {
		candidate := filepath.Join(vDir, guid)
		info, statErr := os.Stat(candidate)
		if statErr != nil || !info.IsDir() {
			continue
		}
		if firstMatch == "" {
			firstMatch = candidate
		}
		mailboxes, discErr := emlx.DiscoverMailboxes(candidate)
		if discErr == nil && len(mailboxes) > 0 {
			return candidate, nil
		}
	}

	if firstMatch != "" {
		return firstMatch, nil
	}
	return "", fmt.Errorf(
		"no directory found for GUID %s in %s", guid, mailDir,
	)
}

// sortedVDirs returns all V* directories in mailDir sorted from
// newest to oldest (e.g. V10, V9, V2).
func sortedVDirs(mailDir string) ([]string, error) {
	entries, err := os.ReadDir(mailDir)
	if err != nil {
		return nil, err
	}

	var vNames []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "V") {
			vNames = append(vNames, e.Name())
		}
	}

	// Sort newest-first.
	sort.Slice(vNames, func(i, j int) bool {
		return versionGreater(vNames[i], vNames[j])
	})

	result := make([]string, len(vNames))
	for i, name := range vNames {
		result[i] = filepath.Join(mailDir, name)
	}
	return result, nil
}

// versionGreater returns true if a > b where both are "V<number>"
// strings. Falls back to lexicographic comparison.
func versionGreater(a, b string) bool {
	na, nb := a[1:], b[1:]
	if len(na) != len(nb) {
		return len(na) > len(nb)
	}
	return na > nb
}
