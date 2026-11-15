package line

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/config"
)

const (
	storeFile  = "Line.sqlite"
	groupsFile = "UnifiedGroup.sqlite"
	walSuffix  = "-wal"
	// manifestFile names the index of an iPhone backup made by Finder,
	// iTunes, or idevicebackup2.
	manifestFile = "Manifest.db"
	// backupDomain is the backup domain of the LINE app group container.
	backupDomain = "AppDomainGroup-group.com.linecorp.line"
	backupPath   = "Library/Application Support/PrivateStore/"
)

// Store reads a LINE iOS message store. Line.sqlite holds chats, users and
// messages; UnifiedGroup.sqlite beside it holds group names and members.
type Store struct {
	// Path is the resolved Line.sqlite file.
	Path string
	// GroupsPath is the resolved UnifiedGroup.sqlite file, or "" when absent.
	GroupsPath    string
	GroupsWALPath string
	// WALPath is the message store's write-ahead log, or "" when it has none.
	WALPath string
	db      *sql.DB
	groups  *sql.DB
	// scratch holds working copies of stores that came with a WAL file; it
	// is removed on Close.
	scratch                                       string
	storeHash, walHash, groupsHash, groupsWALHash string
}

// storeFiles is one database and its write-ahead log, when one exists.
type storeFiles struct {
	db, wal string
}

// Open resolves path to a LINE store and opens it read-only. path may be the
// Line.sqlite file, a directory holding it, or the root of an unencrypted
// iPhone backup, where the store is found through Manifest.db.
func Open(ctx context.Context, path string) (*Store, error) {
	main, groups, err := resolve(ctx, path)
	if err != nil {
		return nil, err
	}
	st := &Store{Path: main.db, GroupsPath: groups.db, GroupsWALPath: groups.wal, WALPath: main.wal}
	if err := st.openAll(ctx, main, groups); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}

func (s *Store) openAll(ctx context.Context, main, groups storeFiles) (err error) {
	s.db, s.storeHash, s.walHash, err = s.open(ctx, main)
	if err != nil {
		return fmt.Errorf("line: open %s: %w", main.db, err)
	}
	if err := checkStoreSchema(ctx, s.db); err != nil {
		return err
	}
	if groups.db != "" {
		s.groups, s.groupsHash, s.groupsWALHash, err = s.open(ctx, groups)
		if err != nil {
			return fmt.Errorf("line: open %s: %w", groups.db, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.groups != nil {
		err = errors.Join(err, s.groups.Close())
	}
	if s.scratch != "" {
		err = errors.Join(err, os.RemoveAll(s.scratch))
	}
	return err
}

// open reads a database without touching it. A store that comes with a
// write-ahead log is copied, log included, to a scratch directory and read
// there, so committed rows the log still holds are seen; SQLite's immutable
// mode, which never looks for a log, reads the original otherwise.
func (s *Store) open(ctx context.Context, files storeFiles) (*sql.DB, string, string, error) {
	if files.wal == "" {
		hash, err := fileHash(files.db)
		if err != nil {
			return nil, "", "", err
		}
		db, err := openImmutable(ctx, files.db)
		return db, hash, "", err
	}
	if s.scratch == "" {
		dir, err := config.MkTempDir("msgvault-line-")
		if err != nil {
			return nil, "", "", err
		}
		s.scratch = dir
	}
	copyTo := filepath.Join(s.scratch, filepath.Base(files.db))
	dbHash, err := copyFile(files.db, copyTo)
	if err != nil {
		return nil, "", "", err
	}
	walHash, err := copyFile(files.wal, copyTo+walSuffix)
	if err != nil {
		return nil, "", "", err
	}
	db, err := openCopy(ctx, copyTo)
	return db, dbHash, walHash, err
}

func copyFile(from, to string) (string, error) {
	src, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		_ = dst.Close()
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolve finds the message store and, when present, the group store and
// their write-ahead logs.
func resolve(ctx context.Context, path string) (main, groups storeFiles, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return main, groups, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return main, groups, fmt.Errorf("line: %w", err)
	}
	if !info.IsDir() {
		main, err = withWAL(abs)
		if err != nil {
			return main, groups, err
		}
		groups, err = withWAL(optionalFile(filepath.Join(filepath.Dir(abs), groupsFile)))
		return main, groups, err
	}
	if _, err := os.Stat(filepath.Join(abs, storeFile)); err == nil {
		main, err = withWAL(filepath.Join(abs, storeFile))
		if err != nil {
			return main, groups, err
		}
		groups, err = withWAL(optionalFile(filepath.Join(abs, groupsFile)))
		return main, groups, err
	}
	if _, err := os.Stat(filepath.Join(abs, manifestFile)); err == nil {
		return resolveBackup(ctx, abs)
	}
	return main, groups, fmt.Errorf("line: %s holds neither %s nor %s", abs, storeFile, manifestFile)
}

func optionalFile(path string) string {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	return ""
}

// withWAL pairs a database with a non-empty write-ahead log beside it.
func withWAL(db string) (storeFiles, error) {
	if db == "" {
		return storeFiles{}, nil
	}
	files := storeFiles{db: db}
	path := db + walSuffix
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return files, nil
	}
	if err != nil {
		return files, fmt.Errorf("line: inspect write-ahead log: %w", err)
	}
	if info.Size() > 0 {
		files.wal = path
	}
	return files, nil
}

// resolveBackup finds the LINE store inside an iPhone backup. Backup files
// are stored under the SHA-1 of "<domain>-<relative path>", in a directory
// named by the hash's first two characters. Manifest.db lists them. LINE
// keeps one PrivateStore directory per account that was signed in on the
// phone, so the backup must hold exactly one message store.
func resolveBackup(ctx context.Context, root string) (main, groups storeFiles, err error) {
	manifest, err := openImmutable(ctx, filepath.Join(root, manifestFile))
	if err != nil {
		if sqliteErr, ok := errors.AsType[sqlite3.Error](err); ok && sqliteErr.Code == sqlite3.ErrNotADB {
			return main, groups, errors.New("line: the backup manifest is not readable; encrypted backups are not supported")
		}
		return main, groups, fmt.Errorf("line: open backup manifest: %w", err)
	}
	defer func() { _ = manifest.Close() }()
	rows, err := manifest.QueryContext(ctx, `SELECT fileID, relativePath FROM Files WHERE domain = ? AND relativePath LIKE ?`,
		backupDomain, backupPath+"%/Messages/%")
	if err != nil {
		return main, groups, fmt.Errorf("line: read backup manifest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// byDir maps each account's Messages directory to the backup files of
	// the two stores and their logs, keyed by file name.
	byDir := map[string]map[string]string{}
	for rows.Next() {
		var id, rel string
		if err := rows.Scan(&id, &rel); err != nil {
			return main, groups, err
		}
		name := filepath.Base(rel)
		switch name {
		case storeFile, groupsFile, storeFile + walSuffix, groupsFile + walSuffix:
		default:
			continue
		}
		if len(id) != 40 || strings.Trim(id, "0123456789abcdef") != "" {
			return main, groups, errors.New("line: invalid backup file ID")
		}
		dir := filepath.Dir(rel)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		byDir[dir][name] = filepath.Join(root, id[:2], id)
	}
	if err := rows.Err(); err != nil {
		return main, groups, err
	}
	var dirs []string
	for dir, files := range byDir {
		if files[storeFile] != "" {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	switch len(dirs) {
	case 0:
		return main, groups, fmt.Errorf("line: the backup at %s has no LINE message store", root)
	case 1:
	default:
		return main, groups, fmt.Errorf("line: the backup at %s holds %d LINE message stores, one per account signed in on the phone; use a backup browser to extract the Messages directory for the account you want, including its SQLite files and write-ahead logs, then import the extracted directory", root, len(dirs))
	}
	files := byDir[dirs[0]]
	main = storeFiles{db: files[storeFile]}
	if _, err := os.Stat(main.db); err != nil {
		return main, groups, fmt.Errorf("line: the backup manifest names a LINE store that is missing: %w", err)
	}
	if main.wal, err = listedWAL(files[storeFile+walSuffix]); err != nil {
		return main, groups, err
	}
	if groups.db = files[groupsFile]; groups.db != "" {
		if _, err := os.Stat(groups.db); err != nil {
			return main, groups, fmt.Errorf("line: the backup manifest names a group store that is missing: %w", err)
		}
		if groups.wal, err = listedWAL(files[groupsFile+walSuffix]); err != nil {
			return main, groups, err
		}
	}
	return main, groups, nil
}

// listedWAL returns a write-ahead log the manifest lists, or "" when it lists
// none or the log is empty. A listed log whose file is missing is an error:
// committed rows can live only there.
func listedWAL(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("line: the backup manifest names a write-ahead log that is missing: %w", err)
	}
	if info.Size() == 0 {
		return "", nil
	}
	return path, nil
}

// openImmutable opens a SQLite file without touching it. Backup copies carry
// a WAL header but no -wal or -shm files, which a normal read-only open
// would try to create.
func openImmutable(ctx context.Context, path string) (*sql.DB, error) {
	return openDSN(ctx, path, "immutable=1&_query_only=1")
}

// openCopy opens a scratch copy read-only; SQLite applies the copied
// write-ahead log and keeps its index in the scratch directory.
func openCopy(ctx context.Context, path string) (*sql.DB, error) {
	return openDSN(ctx, path, "mode=ro&_query_only=1")
}

func openDSN(ctx context.Context, path, rawQuery string) (*sql.DB, error) {
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: rawQuery}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func checkStoreSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('ZMESSAGE', 'ZCHAT', 'ZUSER')`)
	if err != nil {
		return fmt.Errorf("line: not a LINE message store: %w", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 3 {
		return errors.New("line: not a LINE message store")
	}
	return nil
}

// OwnerMID returns the owner's LINE user id when the store holds a Keep Memo
// chat, the one chat whose user is the owner, or "" when it does not.
func (s *Store) OwnerMID(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.ZMID FROM ZCHAT c LEFT JOIN ZUSER u ON u.ZMID = c.ZMID
		WHERE c.ZTYPE = 100 AND c.ZMID LIKE 'u%' AND u.Z_PK IS NULL ORDER BY c.Z_PK`)
	if err != nil {
		return "", fmt.Errorf("line: find owner: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var mids []string
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			return "", err
		}
		mids = append(mids, mid)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(mids) != 1 {
		return "", nil
	}
	return mids[0], nil
}

// Users returns every user by ZUSER row id.
func (s *Store) Users(ctx context.Context) (map[int64]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT Z_PK, ZMID, ZNAME, ZCUSTOMNAME, ZADDRESSBOOKNAME FROM ZUSER WHERE ZMID IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("line: read users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	users := map[int64]*User{}
	for rows.Next() {
		var u User
		var name, custom, addressBook sql.NullString
		if err := rows.Scan(&u.RowID, &u.MID, &name, &custom, &addressBook); err != nil {
			return nil, err
		}
		u.Name = displayName(custom.String, addressBook.String, name.String)
		users[u.RowID] = &u
	}
	return users, rows.Err()
}

// Chats returns every chat in row order, with group titles and members from
// UnifiedGroup.sqlite when present and room members from Line.sqlite.
func (s *Store) Chats(ctx context.Context, users map[int64]*User) ([]*Chat, error) {
	members, err := s.chatMembers(ctx, users)
	if err != nil {
		return nil, err
	}
	groups, err := s.unifiedGroups(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT Z_PK, ZTYPE, ZMID FROM ZCHAT WHERE ZMID IS NOT NULL ORDER BY Z_PK`)
	if err != nil {
		return nil, fmt.Errorf("line: read chats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var chats []*Chat
	for rows.Next() {
		var c Chat
		var kind int
		if err := rows.Scan(&c.RowID, &kind, &c.MID); err != nil {
			return nil, err
		}
		c.Kind = chatKinds[kind]
		c.Members = members[c.RowID]
		if g, ok := groups[c.MID]; ok {
			c.Title, c.TitleKnown = g.name, true
			if len(g.members) > 0 {
				c.Members = g.members
			}
		}
		chats = append(chats, &c)
	}
	return chats, rows.Err()
}

func (s *Store) chatMembers(ctx context.Context, users map[int64]*User) (map[int64][]string, error) {
	members := map[int64][]string{}
	link, err := findJoinTable(ctx, s.db, "MEMBERS", "CHATS", "MEMBERS")
	if err != nil || link.table == "" {
		return members, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+link.left+`, `+link.right+` FROM `+link.table+` ORDER BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("line: read chat members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var chat, user int64
		if err := rows.Scan(&chat, &user); err != nil {
			return nil, err
		}
		if u, ok := users[user]; ok {
			members[chat] = append(members[chat], u.MID)
		}
	}
	return members, rows.Err()
}

type unifiedGroup struct {
	name    string
	members []string
}

func (s *Store) unifiedGroups(ctx context.Context) (map[string]*unifiedGroup, error) {
	groups := map[string]*unifiedGroup{}
	if s.groups == nil {
		return groups, nil
	}
	link, err := findJoinTable(ctx, s.groups, "MEMBERCHATUSERS", "GROUPS", "MEMBERCHATUSERS")
	if err != nil {
		return nil, err
	}
	query := `SELECT ZID, ZNAME, NULL FROM ZUNIFIEDGROUP WHERE ZID IS NOT NULL ORDER BY Z_PK`
	if link.table != "" {
		query = `SELECT g.ZID, g.ZNAME, u.ZMID FROM ZUNIFIEDGROUP g
		LEFT JOIN ` + link.table + ` l ON l.` + link.left + ` = g.Z_PK
		LEFT JOIN ZUNIFIEDGROUPUSER u ON u.Z_PK = l.` + link.right + `
		WHERE g.ZID IS NOT NULL ORDER BY g.Z_PK, u.Z_PK`
	}
	rows, err := s.groups.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("line: read groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var name, member sql.NullString
		if err := rows.Scan(&id, &name, &member); err != nil {
			return nil, err
		}
		g, ok := groups[id]
		if !ok {
			g = &unifiedGroup{name: strings.TrimSpace(name.String)}
			groups[id] = g
		}
		if member.Valid && member.String != "" {
			g.members = append(g.members, member.String)
		}
	}
	return groups, rows.Err()
}

// joinTable names a Core Data many-to-many table and its two key columns.
type joinTable struct{ table, left, right string }

// findJoinTable finds the many-to-many table Core Data creates for a
// relationship. Core Data names the table and its columns Z_<entity
// number><relationship name>, and the entity numbers change when an app
// update adds or removes an entity, so only the names after the number are
// fixed. It returns a zero joinTable when the store has no such table.
func findJoinTable(ctx context.Context, db *sql.DB, table, left, right string) (joinTable, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.name GLOB 'Z_[0-9]*' ORDER BY m.name, c.cid`)
	if err != nil {
		return joinTable{}, fmt.Errorf("line: read members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	found := map[string]*joinTable{}
	var names []string
	for rows.Next() {
		var name, column string
		if err := rows.Scan(&name, &column); err != nil {
			return joinTable{}, err
		}
		if !coreDataName(name, table) {
			continue
		}
		jt := found[name]
		if jt == nil {
			jt = &joinTable{table: name}
			found[name] = jt
			names = append(names, name)
		}
		switch {
		case coreDataName(column, left):
			jt.left = column
		case coreDataName(column, right):
			jt.right = column
		}
	}
	if err := rows.Err(); err != nil {
		return joinTable{}, err
	}
	var match joinTable
	for _, name := range names {
		if jt := found[name]; jt.left != "" && jt.right != "" {
			if match.table != "" {
				return joinTable{}, errors.New("line: ambiguous member lists")
			}
			match = *jt
		}
	}
	return match, nil
}

// coreDataName reports whether name is Z_, an entity number, then suffix.
func coreDataName(name, suffix string) bool {
	number, ok := strings.CutPrefix(name, "Z_")
	if !ok {
		return false
	}
	number, ok = strings.CutSuffix(number, suffix)
	return ok && number != "" && strings.Trim(number, "0123456789") == ""
}

// Messages visits a chat's messages in time order.
func (s *Store) Messages(ctx context.Context, chatRowID int64, visit func(*Message) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT Z_PK, ZID, ZCONTENTTYPE, ZTIMESTAMP, ZSENDER, ZTEXT, ZCONTENTMETADATA
		FROM ZMESSAGE WHERE ZCHAT = ? ORDER BY ZTIMESTAMP, Z_PK`, chatRowID)
	if err != nil {
		return fmt.Errorf("line: read messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m Message
		var id, text sql.NullString
		var contentType, millis, sender sql.NullInt64
		var blob []byte
		if err := rows.Scan(&m.RowID, &id, &contentType, &millis, &sender, &text, &blob); err != nil {
			return err
		}
		m.ID = strings.TrimSpace(id.String)
		m.ContentType = int(contentType.Int64)
		if millis.Valid {
			m.SentAt = time.UnixMilli(millis.Int64).UTC()
		}
		m.SenderRowID = sender.Int64
		m.Text = text.String
		if err := m.readMetadata(blob); err != nil {
			return fmt.Errorf("line: message %q: %w", m.ID, err)
		}
		if err := visit(&m); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *Message) readMetadata(blob []byte) error {
	if m.ID == "" || contentKinds[m.ContentType] != kindText {
		return nil
	}
	var err error
	m.Metadata, err = decodeMetadata(blob)
	return err
}
