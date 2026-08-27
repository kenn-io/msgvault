package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

var (
	backupInitRepo   string
	backupCreateRepo string
	backupListRepo   string
	backupVerifyRepo string

	backupCreateIncludeConfig         bool
	backupCreateIncludeTokens         bool
	backupCreateAllowPlaintextSecrets bool
	backupCreateTag                   string
	backupCreateForceUnlock           bool
	backupCreateJobs                  int

	backupVerifyAll         bool
	backupVerifyQuick       bool
	backupVerifyForceUnlock bool
	backupVerifyJobs        int

	backupRestoreRepo             string
	backupRestoreTarget           string
	backupRestoreOverwrite        bool
	backupRestoreForceUnlock      bool
	backupRestoreJobs             int
	backupRestoreLooseAttachments bool
	backupRestoreIntegrityCheck   bool

	// backupCreateProgress selects backup create's progress rendering mode:
	// auto (default), bar, or plain. It is hidden/undocumented — see
	// resolveClientBackupProgressFlag in backup_progress.go for why it exists
	// at all (the daemon-proxied subprocess can't detect the real terminal).
	backupCreateProgress string
)

// backupRestoreAfterDaemonPreflight is a narrow test barrier for exercising
// ownership races between the early diagnostic check and Kit's authoritative
// target coordination. Production leaves it nil.
var backupRestoreAfterDaemonPreflight func()

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Back up the archive to a snapshot repository",
}

var backupInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new backup repository",
	Args:  cobra.NoArgs,
	RunE:  runBackupInit,
}

var backupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a backup snapshot",
	Args:  cobra.NoArgs,
	RunE:  runBackupCreate,
}

var backupListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List backup snapshots",
	Args:  cobra.NoArgs,
	RunE:  runBackupList,
}

var backupVerifyCmd = &cobra.Command{
	Use:   "verify [SNAPSHOT]",
	Short: "Verify backup repository integrity",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runBackupVerify,
}

var backupRestoreCmd = &cobra.Command{
	Use:   "restore [SNAPSHOT]",
	Short: "Restore a snapshot into a target directory",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runBackupRestore,
}

// resolveBackupRepo applies the standard --repo precedence for every backup
// subcommand: an explicit flag wins, else the configured [backup] repo,
// else an error naming both ways to set it.
func resolveBackupRepo(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if cfg != nil && cfg.Backup.Repo != "" {
		return cfg.Backup.Repo, nil
	}
	return "", errors.New("backup: no repository configured; pass --repo or set [backup] repo in config.toml")
}

func runBackupInit(cmd *cobra.Command, _ []string) error {
	repo, err := resolveBackupRepo(backupInitRepo)
	if err != nil {
		return err
	}
	r, err := backup.Init(repo)
	if err != nil {
		return fmt.Errorf("initializing backup repository: %w", err)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Initialized backup repository %s at %s\n",
		r.Config().RepoID, r.Root()); err != nil {
		return fmt.Errorf("write backup init output: %w", err)
	}
	return nil
}

func runBackupList(cmd *cobra.Command, _ []string) error {
	repo, err := resolveBackupRepo(backupListRepo)
	if err != nil {
		return err
	}
	r, err := backup.Open(repo)
	if err != nil {
		return fmt.Errorf("opening backup repository: %w", err)
	}
	snapshots, err := r.ListSnapshots()
	if err != nil {
		return fmt.Errorf("listing snapshots: %w", err)
	}
	return printBackupSnapshots(cmd.OutOrStdout(), snapshots)
}

func printBackupSnapshots(w io.Writer, snapshots []*backup.Manifest) error {
	if len(snapshots) == 0 {
		if _, err := fmt.Fprintln(w, "No snapshots found."); err != nil {
			return fmt.Errorf("write backup list output: %w", err)
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SNAPSHOT\tCREATED\tMESSAGES\tDOCUMENT TEXT\tBYTES ADDED\tTAG")
	for _, m := range snapshots {
		tag := m.Options.Tag
		if tag == "" {
			tag = "-"
		}
		st, err := backupapp.ParseStats(m.Stats)
		if err != nil {
			return fmt.Errorf("snapshot %s: parsing manifest stats: %w", m.SnapshotID, err)
		}
		documentText := "no"
		if backupapp.ManifestContainsDocumentPlaintext(m) {
			documentText = "normalized plaintext"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.SnapshotID, m.CreatedAt, formatCount(st.Messages), documentText, formatSize(m.BytesAdded), tag)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write backup list output: %w", err)
	}
	return nil
}

func runBackupVerify(cmd *cobra.Command, args []string) error {
	repo, err := resolveBackupRepo(backupVerifyRepo)
	if err != nil {
		return err
	}
	r, err := backup.Open(repo)
	if err != nil {
		return fmt.Errorf("opening backup repository: %w", err)
	}
	var snapshotID string
	if len(args) > 0 {
		snapshotID = args[0]
	}
	// backup verify never proxies through the daemon (cliRunCommandAllowed
	// only admits "backup create"), so cmd.OutOrStdout() here is always the
	// real end-user process's own stdout: auto-detection is safe without a
	// --progress flag to route it through a subprocess boundary.
	renderer := newBackupProgressRenderer(cmd.OutOrStdout(), progressModeAuto)
	// An error mid-stage leaves the in-place TTY line open; close it so the
	// error prints on its own row.
	defer renderer.finish()
	result, err := backup.Verify(cmd.Context(), r, backupapp.New(Version), backup.VerifyOptions{
		SnapshotID:  snapshotID,
		All:         backupVerifyAll,
		Quick:       backupVerifyQuick,
		ForceUnlock: backupVerifyForceUnlock,
		Jobs:        backupVerifyJobs,
		Progress:    renderer.handle,
	})
	if err != nil {
		return fmt.Errorf("verifying backup repository: %w", err)
	}
	for _, p := range result.Problems {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "problem: snapshot %s: %s\n", p.SnapshotID, p.Detail)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "verified %d snapshots, %d blobs; %d problems\n",
		len(result.Snapshots), result.BlobsChecked, len(result.Problems))
	if len(result.Problems) > 0 {
		return fmt.Errorf("backup verify: found %d problem(s)", len(result.Problems))
	}
	return nil
}

// runBackupRestore materializes a snapshot into --target and verifies the
// result. Like verify, it never proxies through the daemon: it reads only
// the repository and writes only the target, never the live archive.
func runBackupRestore(cmd *cobra.Command, args []string) error {
	repo, err := resolveBackupRepo(backupRestoreRepo)
	if err != nil {
		return err
	}
	r, err := backup.Open(repo)
	if err != nil {
		return fmt.Errorf("opening backup repository: %w", err)
	}
	if err := refuseRestoreIntoLiveDaemonHome(backupRestoreTarget); err != nil {
		return err
	}
	if backupRestoreAfterDaemonPreflight != nil {
		backupRestoreAfterDaemonPreflight()
	}
	targetCoordinator, coordinatedTarget, err := backupRestoreTargetCoordinator(
		backupRestoreTarget, backupRestoreOverwrite,
	)
	if err != nil {
		return err
	}
	var targetCoordinatorOption backup.RestoreTargetCoordinator
	if targetCoordinator != nil {
		targetCoordinatorOption = targetCoordinator
	}
	var snapshotID string
	if len(args) > 0 {
		snapshotID = args[0]
	}
	renderer := newBackupProgressRenderer(cmd.OutOrStdout(), progressModeAuto)
	defer renderer.finish()
	looseAttachments := backupRestoreLooseAttachments || cfg.Data.LooseAttachments
	res, err := backup.Restore(cmd.Context(), r, backupapp.New(Version), backup.RestoreOptions{
		SnapshotID:         snapshotID,
		TargetDir:          backupRestoreTarget,
		Overwrite:          backupRestoreOverwrite || coordinatedTarget,
		Jobs:               backupRestoreJobs,
		ForceUnlock:        backupRestoreForceUnlock,
		SkipIntegrityCheck: !backupRestoreIntegrityCheck,
		Progress:           renderer.handle,
		PackedContent:      backupRestorePackedContentTarget(looseAttachments),
		TargetCoordinator:  targetCoordinatorOption,
		AuxiliaryTarget:    backupapp.NewDocumentAuxiliaryTarget(),
		BeforePublication:  backupapp.InvalidateRestoredDocumentVectors,
	})
	if err != nil {
		return fmt.Errorf("restoring snapshot: %w", err)
	}
	return printBackupRestoreSummary(cmd.OutOrStdout(), backupRestoreTarget, res, looseAttachments)
}

func removeSQLiteVectorBackendPath(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup restore: open vector backend directory for reset: %w", err)
	}
	defer func() { _ = root.Close() }()
	return removeSQLiteVectorBackend(root, filepath.Base(path))
}

func removeSQLiteVectorBackend(root *os.Root, name string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		fileName := name + suffix
		if err := root.Remove(fileName); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup restore: remove excluded vector backend %s: %w", fileName, err)
		}
	}
	return nil
}

func backupRestorePackedContentTarget(loose bool) backup.PackedContentTarget {
	limits := packstore.DefaultLimits()
	if loose {
		// Every real pack exceeds one byte, so Kit restores every selected
		// attachment loose and atomically replaces staged pack authority with
		// an empty catalog before publishing the database.
		limits.PackBytes = 1
	}
	return backupapp.NewPackedRestoreTarget(limits)
}

func backupRestoreTargetCoordinator(
	target string,
	overwrite bool,
) (*daemonRestoreTargetCoordinator, bool, error) {
	if cfg == nil || target == "" || cfg.Data.DataDir == "" {
		return nil, false, nil
	}
	databasePath := ""
	configuredVectorPath := ""
	if !store.IsPostgresURL(cfg.DatabaseDSN()) {
		var err error
		databasePath, err = cfg.DatabasePath()
		if err != nil {
			return nil, false, fmt.Errorf("backup restore: resolving configured database: %w", err)
		}
		configuredVectorPath = cfg.Vector.DBPath
		if configuredVectorPath == "" {
			configuredVectorPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
		}
	}
	coordinator := &daemonRestoreTargetCoordinator{
		dataDir:              cfg.Data.DataDir,
		databasePath:         databasePath,
		configuredVectorPath: configuredVectorPath,
		target:               target,
		overwrite:            overwrite,
	}
	mayNeedCoordination, err := restorePathsMayBeEquivalent(target, cfg.Data.DataDir)
	if err != nil {
		return nil, false, fmt.Errorf("backup restore: compare target with configured data directory: %w", err)
	}
	if !mayNeedCoordination && databasePath != "" {
		restoredDatabasePath := filepath.Join(target, backupapp.New(Version).DBFileName())
		mayNeedCoordination, err = restorePathsMayBeEquivalent(restoredDatabasePath, databasePath)
		if err != nil {
			return nil, false, fmt.Errorf("backup restore: compare target database with configured database: %w", err)
		}
	}
	if !mayNeedCoordination {
		if overwrite {
			return coordinator, false, nil
		}
		return nil, false, nil
	}
	if overwrite && configuredVectorPath != "" {
		restoredDatabasePath := filepath.Join(target, backupapp.New(Version).DBFileName())
		matchesDatabase, err := restorePathsMayBeEquivalent(configuredVectorPath, restoredDatabasePath)
		if err != nil {
			return nil, false, fmt.Errorf(
				"backup restore: compare vector database with restored archive database: %w",
				err,
			)
		}
		if matchesDatabase {
			return nil, false, errors.New(
				"backup restore: vector database path resolves to the restored archive database",
			)
		}
	}
	return coordinator, true, nil
}

type daemonRestoreTargetCoordinator struct {
	dataDir              string
	databasePath         string
	configuredVectorPath string
	target               string
	overwrite            bool
}

type pinnedRestoreTargetKind uint8

const (
	pinnedRestoreTargetUnrelated pinnedRestoreTargetKind = iota
	pinnedRestoreTargetDataDir
	pinnedRestoreTargetDatabaseDir
)

func (c *daemonRestoreTargetCoordinator) AcquireRestoreTarget(
	ctx context.Context,
	root *os.Root,
) (backup.RestoreTargetLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	targetKind, err := c.classifyPinnedRestoreTarget(root)
	if err != nil {
		return nil, err
	}
	lease, err := c.newRestoreTargetLease(root, targetKind)
	if err != nil {
		return nil, err
	}
	if targetKind == pinnedRestoreTargetUnrelated {
		if err := c.validateRestoreTargetContents(root, false); err != nil {
			return nil, err
		}
		return lease, nil
	}

	lockRoot := root
	closeLockRoot := false
	if targetKind != pinnedRestoreTargetDataDir {
		if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create configured data directory for restore exclusion: %w", err)
		}
		lockRoot, err = os.OpenRoot(c.dataDir)
		if err != nil {
			return nil, fmt.Errorf("pin configured data directory for restore exclusion: %w", err)
		}
		closeLockRoot = true
		defer func() {
			if closeLockRoot {
				_ = lockRoot.Close()
			}
		}()
	}
	for _, name := range []string{daemonOwnerLockFile, writeOwnerLockFile} {
		lockFile, err := lockRoot.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create restore exclusion lock %s: %w", name, err)
		}
		if err := lockFile.Close(); err != nil {
			return nil, fmt.Errorf("close restore exclusion lock file %s: %w", name, err)
		}
	}
	// Match daemon startup's global lock order so restore cannot deadlock
	// against another process that needs both ownership protocols.
	lease.daemonOwner, err = tryAcquireDaemonOwnerLock(c.dataDir)
	if err != nil {
		return nil, fmt.Errorf("acquire daemon exclusion for restore: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = lease.Release()
		}
	}()
	lease.writeOwner, err = tryAcquireWriteOwnerLock(c.dataDir)
	if err != nil {
		return nil, fmt.Errorf("acquire direct-writer exclusion for restore: %w", err)
	}
	locks := []struct {
		name string
		path string
	}{
		{name: daemonOwnerLockFile, path: daemonOwnerLockPath(c.dataDir)},
		{name: writeOwnerLockFile, path: writeOwnerLockPath(c.dataDir)},
	}
	for _, lock := range locks {
		rootLockInfo, statErr := lockRoot.Stat(lock.name)
		if statErr != nil {
			return nil, fmt.Errorf("inspect pinned restore exclusion lock %s: %w", lock.name, statErr)
		}
		pathLockInfo, statErr := os.Stat(lock.path)
		if statErr != nil {
			return nil, fmt.Errorf("inspect configured restore exclusion lock %s: %w", lock.path, statErr)
		}
		if !os.SameFile(rootLockInfo, pathLockInfo) {
			return nil, fmt.Errorf(
				"configured archive home changed while acquiring restore exclusion lock %s",
				lock.name,
			)
		}
	}
	if closeLockRoot {
		if err := lockRoot.Close(); err != nil {
			return nil, fmt.Errorf("close configured data-directory root after lock verification: %w", err)
		}
		closeLockRoot = false
	}
	if err := c.validateRestoreTargetContents(root, targetKind == pinnedRestoreTargetDataDir); err != nil {
		return nil, err
	}
	success = true
	return lease, nil
}

func (c *daemonRestoreTargetCoordinator) newRestoreTargetLease(
	root *os.Root,
	targetKind pinnedRestoreTargetKind,
) (*daemonRestoreTargetLease, error) {
	databaseFileName := backupapp.New(Version).DBFileName()
	databaseFile, err := root.Open(databaseFileName)
	databaseExisted := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect database before restore publication: %w", err)
	}
	var databaseInfo fs.FileInfo
	if databaseExisted {
		databaseInfo, err = databaseFile.Stat()
		closeErr := databaseFile.Close()
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("inspect database before restore publication: %w", err),
				closeErr,
			)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close database after restore inspection: %w", closeErr)
		}
	}
	lease := &daemonRestoreTargetLease{
		targetRoot:        root,
		databaseFileName:  databaseFileName,
		databaseInfo:      databaseInfo,
		databaseExisted:   databaseExisted,
		resetTargetVector: c.overwrite,
	}
	if c.overwrite && targetKind != pinnedRestoreTargetUnrelated {
		lease.configuredVectorPath = c.configuredVectorPath
	}
	return lease, nil
}

func (c *daemonRestoreTargetCoordinator) classifyPinnedRestoreTarget(
	root *os.Root,
) (pinnedRestoreTargetKind, error) {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return pinnedRestoreTargetUnrelated, fmt.Errorf("inspect pinned restore target: %w", err)
	}
	dataDirInfo, err := os.Stat(c.dataDir)
	if err == nil {
		if os.SameFile(rootInfo, dataDirInfo) {
			return pinnedRestoreTargetDataDir, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return pinnedRestoreTargetUnrelated, fmt.Errorf("inspect configured data directory: %w", err)
	}
	if c.databasePath == "" {
		return pinnedRestoreTargetUnrelated, nil
	}

	dbFileName := backupapp.New(Version).DBFileName()
	rootDBInfo, rootDBErr := root.Stat(dbFileName)
	configuredDBInfo, configuredDBErr := os.Stat(c.databasePath)
	if rootDBErr == nil && configuredDBErr == nil && os.SameFile(rootDBInfo, configuredDBInfo) {
		return pinnedRestoreTargetDatabaseDir, nil
	}
	if rootDBErr != nil && !errors.Is(rootDBErr, os.ErrNotExist) {
		return pinnedRestoreTargetUnrelated, fmt.Errorf("inspect database in pinned restore target: %w", rootDBErr)
	}
	if configuredDBErr != nil && !errors.Is(configuredDBErr, os.ErrNotExist) {
		return pinnedRestoreTargetUnrelated, fmt.Errorf("inspect configured database: %w", configuredDBErr)
	}

	databaseParentInfo, err := os.Stat(filepath.Dir(c.databasePath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pinnedRestoreTargetUnrelated, nil
		}
		return pinnedRestoreTargetUnrelated, fmt.Errorf("inspect configured database directory: %w", err)
	}
	if os.SameFile(rootInfo, databaseParentInfo) &&
		strings.EqualFold(dbFileName, filepath.Base(c.databasePath)) {
		return pinnedRestoreTargetDatabaseDir, nil
	}
	return pinnedRestoreTargetUnrelated, nil
}

func (c *daemonRestoreTargetCoordinator) validateRestoreTargetContents(
	root *os.Root,
	allowLockFiles bool,
) error {
	if c.overwrite {
		return nil
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("inspect coordinated restore target: %w", err)
	}
	for _, entry := range entries {
		if allowLockFiles && (entry.Name() == daemonOwnerLockFile || entry.Name() == writeOwnerLockFile) {
			continue
		}
		return fmt.Errorf(
			"restore target %s is not empty (use --overwrite to restore into it anyway)",
			c.target,
		)
	}
	return nil
}

type daemonRestoreTargetLease struct {
	daemonOwner          *daemonOwnerLock
	writeOwner           *writeOwnerLock
	targetRoot           *os.Root
	databaseFileName     string
	databaseInfo         fs.FileInfo
	databaseExisted      bool
	resetTargetVector    bool
	configuredVectorPath string
}

func (l *daemonRestoreTargetLease) Release() error {
	if l == nil {
		return nil
	}
	writeOwner := l.writeOwner
	daemonOwner := l.daemonOwner
	targetRoot := l.targetRoot
	l.writeOwner = nil
	l.daemonOwner = nil
	l.targetRoot = nil
	cleanupErr := l.removePublishedVectorBackends(targetRoot)
	writeErr := writeOwner.Close()
	daemonErr := daemonOwner.Close()
	return errors.Join(cleanupErr, writeErr, daemonErr)
}

func (l *daemonRestoreTargetLease) removePublishedVectorBackends(root *os.Root) error {
	if root == nil || !l.resetTargetVector {
		return nil
	}
	currentInfo, err := root.Stat(l.databaseFileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect database after restore publication: %w", err)
	}
	if l.databaseExisted && os.SameFile(l.databaseInfo, currentInfo) {
		return nil
	}
	if l.configuredVectorPath != "" {
		configuredInfo, statErr := os.Stat(l.configuredVectorPath)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect configured vector database before restore cleanup: %w", statErr)
		}
		if statErr == nil && os.SameFile(configuredInfo, currentInfo) {
			return errors.New(
				"backup restore: vector database path resolves to the restored archive database",
			)
		}
	}
	cleanupErr := removeSQLiteVectorBackend(root, "vectors.db")
	if l.configuredVectorPath != "" {
		cleanupErr = errors.Join(cleanupErr, removeSQLiteVectorBackendPath(l.configuredVectorPath))
	}
	return cleanupErr
}

func printBackupRestoreSummary(w io.Writer, target string, res *backup.RestoreResult, looseOnly bool) error {
	if res == nil {
		return errors.New("backup restore result is nil")
	}
	lines := []string{
		fmt.Sprintf("Restored snapshot %s to %s\n", res.SnapshotID, target),
		fmt.Sprintf("Database: %s (%s)\n", res.DBPath, formatSize(res.DBBytes)),
		fmt.Sprintf("Attachments: %d (%s); %d packed in %d pack(s), %d loose\n",
			res.AttachmentBlobs, formatSize(res.AttachmentBytes), res.PackedAttachmentBlobs,
			res.AttachmentPacks, res.LooseAttachmentBlobs),
	}
	if len(res.PackFallbacks) > 0 {
		counts := make(map[string]int)
		for _, fallback := range res.PackFallbacks {
			counts[string(fallback.Reason)]++
		}
		keys := make([]string, 0, len(counts))
		for reason := range counts {
			keys = append(keys, reason)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, reason := range keys {
			parts[i] = fmt.Sprintf("%s=%d", reason, counts[reason])
		}
		lines = append(lines, "Pack fallbacks: "+strings.Join(parts, ", ")+"\n")
	}
	if res.LooseAttachmentBlobs > 0 {
		if looseOnly {
			lines = append(lines,
				"Pack metadata cleared: attachments were restored as loose files\n")
		} else {
			lines = append(lines, fmt.Sprintf(
				"%d attachment blob(s) remain loose; 'msgvault pack-attachments' can pack eligible blobs later\n",
				res.LooseAttachmentBlobs))
		}
	}
	if res.ExtrasFiles > 0 {
		lines = append(lines, fmt.Sprintf("Extras files: %d\n", res.ExtrasFiles))
	}
	verification := "Verification: page and blob hashes verified; manifest stats match\n"
	if res.DatabaseIntegrityChecked {
		verification = "Verification: page and blob hashes verified; SQLite integrity_check ok; manifest stats match\n"
	}
	lines = append(lines, verification, fmt.Sprintf("Duration: %.1fs\n", res.Duration.Seconds()))
	for _, line := range lines {
		if _, err := io.WriteString(w, line); err != nil {
			return fmt.Errorf("write backup restore output: %w", err)
		}
	}
	return nil
}

// refuseRestoreIntoLiveDaemonHome rejects a restore target that is the
// configured archive home while a daemon is running there — the daemon owns
// that SQLite database, and writing under it would corrupt a live archive
// (docs/architecture/backup-format.md, Restore). Any responding daemon or
// held daemon ownership lease counts, including a daemon whose API version or
// process identity this client cannot verify — it owns the database all the
// same. A stopped daemon's home is still non-empty and so requires --overwrite
// like any other directory. Target and home are compared as filesystem
// objects, not path strings, so a case-variant or symlinked spelling of the
// home is refused too.
func refuseRestoreIntoLiveDaemonHome(target string) error {
	configuredHome, err := restoreTargetsConfiguredArchive(target)
	if err != nil {
		return err
	}
	if !configuredHome {
		return nil
	}
	if rt := findAnyDaemonRuntime(cfg.Data.DataDir); rt != nil {
		return fmt.Errorf(
			"backup restore: target %s is the live archive home of a running daemon; stop the daemon first (msgvault daemon stop) or restore elsewhere",
			target)
	}
	ownershipHeld, err := daemonOwnerLockHeld(cfg.Data.DataDir)
	if err != nil {
		return fmt.Errorf("backup restore: inspect daemon ownership: %w", err)
	}
	if ownershipHeld {
		return fmt.Errorf(
			"backup restore: target %s is the live archive home of a running daemon; stop the daemon first (msgvault daemon stop) or restore elsewhere",
			target)
	}
	return nil
}

func restoreTargetsConfiguredArchive(target string) (bool, error) {
	if cfg == nil || target == "" || cfg.Data.DataDir == "" {
		return false, nil
	}
	dataDirMatch, err := restorePathsEquivalent(target, cfg.Data.DataDir)
	if err != nil {
		return false, fmt.Errorf("backup restore: compare target with configured data directory: %w", err)
	}
	if dataDirMatch || store.IsPostgresURL(cfg.DatabaseDSN()) {
		return dataDirMatch, nil
	}
	databasePath, err := cfg.DatabasePath()
	if err != nil {
		return false, fmt.Errorf("backup restore: resolving configured database: %w", err)
	}
	restoredDatabasePath := filepath.Join(target, backupapp.New(Version).DBFileName())
	databaseMatch, err := restorePathsEquivalent(restoredDatabasePath, databasePath)
	if err != nil {
		return false, fmt.Errorf("backup restore: compare target database with configured database: %w", err)
	}
	return databaseMatch, nil
}

func restorePathsEquivalent(a, b string) (bool, error) {
	aCanonical, bCanonical, err := canonicalRestorePaths(a, b)
	if err != nil {
		return false, err
	}
	return aCanonical == bCanonical || sameExistingPath(aCanonical, bCanonical), nil
}

func restorePathsMayBeEquivalent(a, b string) (bool, error) {
	aCanonical, bCanonical, err := canonicalRestorePaths(a, b)
	if err != nil {
		return false, err
	}
	return aCanonical == bCanonical ||
		sameExistingPath(aCanonical, bCanonical) ||
		strings.EqualFold(aCanonical, bCanonical), nil
}

func canonicalRestorePaths(a, b string) (string, string, error) {
	aCanonical, err := canonicalPathThroughExistingAncestor(a)
	if err != nil {
		return "", "", fmt.Errorf("resolving %q: %w", a, err)
	}
	bCanonical, err := canonicalPathThroughExistingAncestor(b)
	if err != nil {
		return "", "", fmt.Errorf("resolving %q: %w", b, err)
	}
	return aCanonical, bCanonical, nil
}

// canonicalPathThroughExistingAncestor resolves symlinks in the deepest
// existing ancestor of path, then appends the still-missing suffix. Unlike
// filepath.EvalSymlinks on the complete path, this identifies equivalent
// future archive locations before restore creates their leaf directories.
func canonicalPathThroughExistingAncestor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	existing := abs
	var missing []string
	for {
		if _, err := os.Stat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	canonical, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for _, component := range slices.Backward(missing) {
		canonical = filepath.Join(canonical, component)
	}
	return canonical, nil
}

// sameExistingPath reports whether a and b name the same existing filesystem
// object even when their spellings differ. filepath.Abs alone compares
// strings, which a case-variant spelling on a case-insensitive filesystem or
// a symlinked path to the archive home would bypass; os.Stat resolves both
// to the object itself. Two paths that do not both exist are not the same
// object — in particular, a live archive home always exists.
func sameExistingPath(a, b string) bool {
	aInfo, err := os.Stat(a)
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(aInfo, bInfo)
}

// runBackupCreate is dual-mode like verify.go's RunE: outside the daemon CLI
// subprocess it proxies the invocation through the daemon (which re-spawns
// this same command inside the subprocess, forwarding every set flag
// verbatim); inside the subprocess it runs the capture locally, bracketed by
// a freeze window held on the parent daemon.
func runBackupCreate(cmd *cobra.Command, args []string) error {
	if !isDaemonCLISubprocess() {
		// The subprocess's own stdout is a pipe back to the daemon, never a
		// real terminal, so its own auto-detection would always fall back to
		// plain. Resolve "auto" here, using the client's own terminal, and
		// forward the resolved value explicitly.
		if err := resolveClientBackupProgressFlag(cmd); err != nil {
			return err
		}
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}
	return runBackupCreateLocal(cmd)
}

// backupExtrasSpec builds the msgvault extras selection for the generic
// backup engine: the deletions directory always rides along; config.toml and
// the tokens directory plus client-secret files are opt-in and marked
// sensitive. The flag-named plaintext guard lives here so users see their
// CLI flags in the error; the engine's own sensitive-source guard is the
// backstop.
func backupExtrasSpec() (backup.ExtrasSpec, error) {
	if (backupCreateIncludeConfig || backupCreateIncludeTokens) && !backupCreateAllowPlaintextSecrets {
		var flag string
		switch {
		case backupCreateIncludeConfig && backupCreateIncludeTokens:
			flag = "--include-config/--include-tokens"
		case backupCreateIncludeConfig:
			flag = "--include-config"
		default:
			flag = "--include-tokens"
		}
		return backup.ExtrasSpec{}, fmt.Errorf(
			"%s requires an encrypted repository (use --allow-plaintext-secrets to override)", flag)
	}
	spec := backup.ExtrasSpec{Dirs: []backup.ExtrasDirSpec{{Name: "deletions"}}}
	if backupCreateIncludeConfig {
		if cfgPath := cfg.ConfigFilePath(); cfgPath != "" {
			spec.Files = append(spec.Files, backup.ExtrasFileSpec{
				Path: cfgPath, RecordAs: "config.toml", Sensitive: true,
			})
		}
	}
	if backupCreateIncludeTokens {
		spec.Dirs = append(spec.Dirs, backup.ExtrasDirSpec{Name: "tokens", Sensitive: true})
		spec.Globs = append(spec.Globs, backup.ExtrasGlobSpec{Pattern: "client_secret*.json", Sensitive: true})
	}
	return spec, nil
}

func runBackupCreateLocal(cmd *cobra.Command) error {
	repo, err := resolveBackupRepo(backupCreateRepo)
	if err != nil {
		return err
	}
	dbPath, err := cfg.DatabasePath()
	if err != nil {
		return err
	}
	r, err := backup.Open(repo)
	if err != nil {
		return fmt.Errorf("opening backup repository: %w", err)
	}

	// Capture reads attachment bytes through the production blob store
	// (packs + loose fallback) rather than the engine's own ContentDir
	// reads, so a packed vault backs up without any loose files. The
	// read-only SQLite connection alongside the daemon's writer is safe
	// under WAL. Kit releases the daemon's operation gate after pinning the
	// database snapshot, before attachment capture, so maintenance can run
	// concurrently; blob-store reads finish through an already-open pack,
	// follow a replacement mapping, or fail loudly and leave the backup
	// retryable.
	roStore, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open archive for backup content reads: %w", err)
	}
	defer func() { _ = roStore.Close() }()
	blobs, err := attachmentstore.New(store.NewPackCatalog(roStore), cfg.AttachmentsDir())
	if err != nil {
		return fmt.Errorf("open attachment content store for backup: %w", err)
	}
	defer func() { _ = blobs.Close() }()

	freezer, closeFreezer, err := newBackupFreezer(cmd.Context())
	if err != nil {
		return err
	}
	defer closeFreezer()

	// By the time execution reaches here, the client-proxy branch of
	// runBackupCreate has already resolved "auto" to a concrete "bar" or
	// "plain" using its own terminal before forwarding this flag; "auto" only
	// reaches this local-mode fallback in a hypothetical direct (non-proxied)
	// invocation, in which case it resolves from this process's own stdout.
	mode, err := backupProgressModeFromFlag(backupCreateProgress)
	if err != nil {
		return err
	}
	renderer := newBackupProgressRenderer(cmd.OutOrStdout(), mode)
	defer renderer.finish()

	extras, err := backupExtrasSpec()
	if err != nil {
		return err
	}
	m, err := backup.Create(cmd.Context(), r, backupapp.New(Version), backup.CreateOptions{
		DBPath:                dbPath,
		ContentDir:            cfg.AttachmentsDir(),
		ContentSource:         backupapp.NewContentSource(blobs, cfg.AttachmentsDir()),
		DataDir:               cfg.Data.DataDir,
		Extras:                extras,
		IncludeConfig:         backupCreateIncludeConfig,
		IncludeTokens:         backupCreateIncludeTokens,
		AllowPlaintextSecrets: backupCreateAllowPlaintextSecrets,
		Tag:                   backupCreateTag,
		ZstdLevel:             cfg.Backup.ZstdLevel,
		CacheDir:              filepath.Join(cfg.HomeDir, "backup-cache"),
		Freezer:               freezer,
		ForceUnlock:           backupCreateForceUnlock,
		Jobs:                  backupCreateJobs,
		Progress:              renderer.handle,
	})
	if err != nil {
		return fmt.Errorf("creating backup snapshot: %w", err)
	}

	parent := m.ParentID
	if parent == "" {
		parent = "initial"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created snapshot %s (parent: %s)\n", m.SnapshotID, parent)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Packs added: %d\n", len(m.NewPacks))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Bytes added: %s\n", formatSize(m.BytesAdded))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Duration: %.1fs\n", m.DurationSeconds)
	return nil
}

// newBackupFreezer resolves the parent daemon's runtime record and builds a
// freezeViaDaemon coordinator over it. backup create must never scan a
// live-daemon-owned SQLite file unfrozen, so a daemon that cannot be
// resolved here is a hard failure rather than a silent unfrozen fallback.
func newBackupFreezer(ctx context.Context) (backup.FreezeCoordinator, func(), error) {
	rt := findDaemonRuntime(cfg.Data.DataDir)
	if rt == nil {
		return nil, func() {}, errors.New(
			"backup create: no running msgvault daemon found; refusing to back up an unfrozen archive")
	}
	// Begin and End deliberately use separate per-call contexts. Keep command
	// values on the client root without letting command cancellation poison the
	// cleanup request that releases an already-open freeze window.
	client, err := newDaemonCLIClient(context.WithoutCancel(ctx), daemonclient.Config{
		URL:           urlFromDaemonRuntime(rt),
		APIKey:        cfg.Server.APIKey,
		AllowInsecure: true,
	})
	if err != nil {
		return nil, func() {}, fmt.Errorf("backup create: connecting to daemon: %w", err)
	}
	return &freezeViaDaemon{client: client}, func() { _ = client.Close() }, nil
}

// freezeViaDaemon implements backup.FreezeCoordinator by brokering the
// freeze window through the parent daemon's HTTP API: Begin opens the
// window and holds the returned token, End closes it with that token.
type freezeViaDaemon struct {
	client *daemonclient.Client
	token  string
}

func (f *freezeViaDaemon) Begin(ctx context.Context) error {
	token, err := f.client.BackupFreezeBegin(ctx)
	if err != nil {
		return err
	}
	f.token = token
	return nil
}

func (f *freezeViaDaemon) End(ctx context.Context) error {
	return f.client.BackupFreezeEnd(ctx, f.token)
}

func init() {
	backupInitCmd.Flags().StringVar(&backupInitRepo, "repo", "", "Backup repository directory")

	backupCreateCmd.Flags().StringVar(&backupCreateRepo, "repo", "", "Backup repository directory")
	backupCreateCmd.Flags().BoolVar(&backupCreateIncludeConfig, "include-config", false, "Include config.toml verbatim (may contain API keys) in the snapshot")
	backupCreateCmd.Flags().BoolVar(&backupCreateIncludeTokens, "include-tokens", false, "Include the tokens directory in the snapshot")
	backupCreateCmd.Flags().BoolVar(&backupCreateAllowPlaintextSecrets, "allow-plaintext-secrets", false, "Allow capturing secrets in plaintext (required with --include-config/--include-tokens on an unencrypted repository)")
	backupCreateCmd.Flags().StringVar(&backupCreateTag, "tag", "", "Optional label recorded on the snapshot manifest")
	backupCreateCmd.Flags().BoolVar(&backupCreateForceUnlock, "force-unlock", false, "Break a stale exclusive repository lock before creating")
	backupCreateCmd.Flags().IntVar(&backupCreateJobs, "jobs", 0, "Concurrent attachment capture workers (default: one per CPU; use 1 for serial reads on spinning disks or NAS shares)")
	backupCreateCmd.Flags().StringVar(&backupCreateProgress, "progress", "auto", "Progress output mode: auto, bar, or plain")
	_ = backupCreateCmd.Flags().MarkHidden("progress")

	backupListCmd.Flags().StringVar(&backupListRepo, "repo", "", "Backup repository directory")

	backupVerifyCmd.Flags().StringVar(&backupVerifyRepo, "repo", "", "Backup repository directory")
	backupVerifyCmd.Flags().BoolVar(&backupVerifyAll, "all", false, "Verify every snapshot instead of only the latest")
	backupVerifyCmd.Flags().BoolVar(&backupVerifyQuick, "quick", false, "Skip reading and hash-verifying content blobs")
	backupVerifyCmd.Flags().BoolVar(&backupVerifyForceUnlock, "force-unlock", false, "Break a stale exclusive repository lock before verifying")
	backupVerifyCmd.Flags().IntVar(&backupVerifyJobs, "jobs", 0, "Concurrent pack readers for full verify (default: one per CPU; use 1 for serial reads on spinning disks or NAS shares)")

	backupRestoreCmd.Flags().StringVar(&backupRestoreRepo, "repo", "", "Backup repository directory")
	backupRestoreCmd.Flags().StringVar(&backupRestoreTarget, "target", "", "Directory to restore into (required)")
	_ = backupRestoreCmd.MarkFlagRequired("target")
	backupRestoreCmd.Flags().BoolVar(&backupRestoreOverwrite, "overwrite", false, "Allow restoring into a non-empty target directory")
	backupRestoreCmd.Flags().BoolVar(&backupRestoreForceUnlock, "force-unlock", false, "Break a stale exclusive repository lock before restoring")
	backupRestoreCmd.Flags().IntVar(&backupRestoreJobs, "jobs", 0, "Concurrent pack readers (default: one per CPU; use 1 for serial reads on spinning disks or NAS shares)")
	backupRestoreCmd.Flags().BoolVar(&backupRestoreIntegrityCheck, "integrity-check", false,
		"Run SQLite's full integrity check after restoring (slow for large databases)")
	backupRestoreCmd.Flags().BoolVar(&backupRestoreLooseAttachments, "loose-attachments", false,
		"Restore attachments as loose files instead of installing compatible packs")

	backupCmd.AddCommand(backupInitCmd)
	backupCmd.AddCommand(backupCreateCmd)
	backupCmd.AddCommand(backupListCmd)
	backupCmd.AddCommand(backupVerifyCmd)
	backupCmd.AddCommand(backupRestoreCmd)
	rootCmd.AddCommand(backupCmd)
}
