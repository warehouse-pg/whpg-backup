package backup

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/nightlyone/lockfile"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/warehouse-pg/common-go-libs/cluster"
	"github.com/warehouse-pg/common-go-libs/gplog"
	"github.com/warehouse-pg/common-go-libs/operating"
)

// Sentinel values written into BackupConfig.DateDeleted when a delete attempt didn't leave the
// backup fully removed. Anything else non-empty is a completed deletion timestamp.
const (
	deleteStatusInProgress   = "In progress"
	deleteStatusPluginFailed = "Plugin Backup Delete Failed"
	deleteStatusLocalFailed  = "Local Delete Failed"
)

// coordinatorDataDirEnvVars mirrors the two names GPDB tooling uses for the coordinator's
// data directory across versions; newer installs export the first, older ones the second.
var coordinatorDataDirEnvVars = []string{"COORDINATOR_DATA_DIRECTORY", "MASTER_DATA_DIRECTORY"}

func getCoordinatorDataDir() (string, error) {
	for _, envVar := range coordinatorDataDirEnvVars {
		if dir := operating.System.Getenv(envVar); dir != "" {
			return dir, nil
		}
	}
	return "", errors.Errorf("%s must be set to locate the backup history database",
		coordinatorDataDirEnvVars[0])
}

func isFullyDeleted(dateDeleted string) bool {
	switch dateDeleted {
	case "", deleteStatusInProgress, deleteStatusPluginFailed, deleteStatusLocalFailed:
		return false
	default:
		return true
	}
}

// checkNotBackupInProgress errors if bc is still being written by a live gpbackup run.
//
// Status alone can't tell: SIGKILL skips the defer/recover that would set Status to
// Success/Failure, so a crashed run leaves BackupStatusInProgress stuck forever. Probing the
// lock file (always under /tmp; --no-history runs are never recorded here) disambiguates —
// TryLock cleans up and succeeds when the owning pid is dead, which is the crash case this
// should let through.
func checkNotBackupInProgress(bc *history.BackupConfig) error {
	if bc.Status != history.BackupStatusInProgress {
		return nil
	}

	lock, err := lockfile.New(fmt.Sprintf("/tmp/%s.lck", bc.Timestamp))
	if err != nil {
		return errors.Errorf("Backup %s is marked in progress and its lock file could not be checked: %s", bc.Timestamp, err.Error())
	}
	if err := lock.TryLock(); err != nil {
		return errors.Errorf("Backup %s is still in progress; wait for it to complete before deleting", bc.Timestamp)
	}
	_ = lock.Unlock()
	return nil
}

func formatHistoryTimestamp(ts string) string {
	t, err := time.ParseInLocation("20060102150405", ts, operating.System.Local)
	if err != nil {
		return ts
	}
	return t.Format("Mon Jan 2 2006 15:04:05")
}

// shellQuote wraps s in single quotes so it round-trips through a shell (bash -c locally, ssh
// remotely) as one argument regardless of spaces or other special characters.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// backupSegPrefix discovers bc's on-disk segment-directory prefix (e.g. "gpseg") from the
// existing directory layout, since delete-backup has no live connection to call
// filepath.GetSegPrefix. Only relevant when --backup-dir was used without --single-backup-dir.
func backupSegPrefix(bc *history.BackupConfig) (string, error) {
	if bc.BackupDir == "" || bc.SingleBackupDir {
		return "", nil
	}
	segPrefix, _, err := filepath.ParseSegPrefix(bc.BackupDir, bc.Timestamp)
	if err != nil {
		return "", err
	}
	return segPrefix, nil
}

// Must run before flag parsing.
func DoDeleteBackupInit(cmd *cobra.Command) {
	RegisterDeleteBackupFlags(cmd.Flags())
}

func DoDeleteBackup(timestamp string) {
	SetLoggerVerbosity()

	if !filepath.IsValidTimestamp(timestamp) {
		gplog.Fatal(errors.Errorf("Invalid timestamp: %s", timestamp), "")
	}

	coordinatorDataDir, err := getCoordinatorDataDir()
	gplog.FatalOnError(err)

	historyDB, err := openBackupHistoryDatabase(coordinatorDataDir)
	gplog.FatalOnError(err)
	defer historyDB.Close()

	pluginConfigPath := MustGetFlagString(options.PLUGIN_CONFIG)
	pluginConfig, segCluster := setupDeletionTargets(coordinatorDataDir, pluginConfigPath)

	deleted, err := deleteBackupChain(historyDB, timestamp, deleteChainOptions{
		cascade:            MustGetFlagBool(options.CASCADE),
		cascadeSupported:   true,
		noPrompt:           MustGetFlagBool(options.NO_PROMPT),
		pluginConfigPath:   pluginConfigPath,
		pluginConfig:       pluginConfig,
		segCluster:         segCluster,
		coordinatorDataDir: coordinatorDataDir,
		stdinReader:        bufio.NewReader(os.Stdin),
	})
	gplog.FatalOnError(err)
	if deleted > 0 {
		gplog.Info("Successfully deleted %d backup(s)", deleted)
	}
}

// openBackupHistoryDatabase locates and opens the coordinator's backup history sqlite db.
func openBackupHistoryDatabase(coordinatorDataDir string) (*sql.DB, error) {
	// GetBackupHistoryDatabasePath only reads SegDirMap[-1], so there is no need to query the
	// cluster for a full FilePathInfo to just locate the history db.
	fpInfo := filepath.FilePathInfo{SegDirMap: map[int]string{-1: coordinatorDataDir}}
	historyDBPath := fpInfo.GetBackupHistoryDatabasePath()
	if _, err := os.Stat(historyDBPath); os.IsNotExist(err) {
		return nil, errors.Errorf("No backup history database found at %s", historyDBPath)
	}
	return history.InitializeHistoryDatabase(historyDBPath)
}

// setupDeletionTargets builds the plugin config and/or cluster topology needed to actually
// remove backup files, shared by every delete-backup(-before) invocation.
func setupDeletionTargets(coordinatorDataDir string, pluginConfigPath string) (*utils.PluginConfig, *cluster.Cluster) {
	var pluginConfig *utils.PluginConfig
	if pluginConfigPath != "" {
		var err error
		pluginConfig, err = utils.ReadPluginConfig(pluginConfigPath)
		gplog.FatalOnError(err)
		// DeleteBackup execs the plugin locally only, so skip ReadPluginConfig's /tmp copy (never
		// populated here) and use the given path directly.
		pluginConfig.ConfigPath = pluginConfigPath
	}

	// Only local (non-plugin) backups need cluster topology to find files to remove.
	// GetSegmentConfigurationFromFile reads the FTS-maintained gpsegconfig_dump on disk, so this
	// works even when the database is down.
	var segCluster *cluster.Cluster
	if pluginConfig == nil {
		segConfigs, err := cluster.GetSegmentConfigurationFromFile(coordinatorDataDir)
		gplog.FatalOnError(err)
		segCluster = cluster.NewCluster(segConfigs)
	}
	return pluginConfig, segCluster
}

type deleteChainOptions struct {
	cascade bool
	// cascadeSupported controls whether a blocked-by-dependents error suggests --cascade;
	// delete-backups-before has no such flag, so it leaves this false.
	cascadeSupported bool
	// skipIncremental makes deleteBackupChain refuse to delete an incremental backup outright,
	// regardless of dependents. delete-backups-before sets this so its unattended sweep never
	// removes part of an incremental chain; delete-backup (explicit, user-confirmed) leaves it
	// false so --cascade can still delete incrementals on request.
	skipIncremental    bool
	noPrompt           bool
	pluginConfigPath   string
	pluginConfig       *utils.PluginConfig
	segCluster         *cluster.Cluster
	coordinatorDataDir string
	// stdinReader is shared across every backup in a delete-backups-before run so a piped
	// multi-line answer (e.g. "y\ny\ny\n") isn't lost inside a bufio.Reader that gets discarded
	// after one prompt.
	stdinReader *bufio.Reader
}

// deleteBackupChain resolves and deletes the backup at timestamp, plus (with cascade) everything
// that depends on it. It is the same pipeline DoDeleteBackup runs for a single CLI invocation,
// extracted so delete-backups-before can call it once per candidate timestamp; errors are
// returned rather than fatal so one blocked/failed backup doesn't need to stop the whole loop.
// Returns the number of backups actually deleted (0 if already deleted or the user declined the
// prompt).
func deleteBackupChain(historyDB *sql.DB, timestamp string, opts deleteChainOptions) (int, error) {
	target, err := history.GetBackupConfig(timestamp, historyDB)
	if err != nil {
		return 0, err
	}
	if isFullyDeleted(target.DateDeleted) {
		gplog.Info("Backup %s has already been deleted", timestamp)
		return 0, nil
	}
	if err := checkNotBackupInProgress(target); err != nil {
		return 0, err
	}
	if opts.skipIncremental && target.Incremental {
		return 0, errors.Errorf("Backup %s is an incremental backup; delete-backups-before does not delete incremental backups", timestamp)
	}

	backupsToDelete, err := resolveDeletionOrder(historyDB, target, opts.cascade, opts.cascadeSupported)
	if err != nil {
		return 0, err
	}

	if err := validatePluginConfigMatch(backupsToDelete, opts.pluginConfigPath); err != nil {
		return 0, err
	}

	if !opts.noPrompt && !promptForDeletion(opts.stdinReader, backupsToDelete) {
		gplog.Info("Backup deletion cancelled for %s", timestamp)
		return 0, nil
	}

	// Re-resolve right before mutating anything, in case a concurrent `gpbackup --incremental`
	// chained onto timestamp while we were preparing.
	recheck, err := resolveDeletionOrder(historyDB, target, opts.cascade, opts.cascadeSupported)
	if err != nil {
		return 0, err
	}
	if added := newlyAddedDependents(backupsToDelete, recheck); len(added) > 0 {
		return 0, errors.Errorf(
			"Backup %s gained new dependent(s) (%s) while delete-backup was preparing; re-run delete-backup to pick them up",
			timestamp, strings.Join(added, ", "))
	}

	for _, bc := range backupsToDelete {
		deleteOneBackup(historyDB, bc, opts.pluginConfig, opts.segCluster, opts.coordinatorDataDir)
	}
	return len(backupsToDelete), nil
}

// Returns dependents (transitive, not-yet-deleted) sorted newest-first, followed by target
// itself last, or an error if dependents exist and cascade is false. cascadeSupported controls
// only the wording of that error: callers without a --cascade flag of their own (e.g.
// delete-backups-before) get pointed at "delete-backup --cascade" by name instead of at a bare
// --cascade flag they don't have.
func resolveDeletionOrder(historyDB *sql.DB, target *history.BackupConfig, cascade bool, cascadeSupported bool) ([]*history.BackupConfig, error) {
	timestamp := target.Timestamp
	visited := map[string]bool{timestamp: true}
	dependents := make([]*history.BackupConfig, 0)
	queue := []string{timestamp}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		deps, err := history.GetBackupDependents(historyDB, current)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			if visited[dep] {
				continue
			}
			bc, err := history.GetBackupConfig(dep, historyDB)
			if err != nil {
				return nil, err
			}
			if isFullyDeleted(bc.DateDeleted) {
				// Already gone; it no longer needs this backup's files.
				continue
			}
			if err := checkNotBackupInProgress(bc); err != nil {
				return nil, err
			}
			visited[dep] = true
			dependents = append(dependents, bc)
			queue = append(queue, dep)
		}
	}

	if len(dependents) > 0 && !cascade {
		names := make([]string, len(dependents))
		for i, bc := range dependents {
			names[i] = bc.Timestamp
		}
		if !cascadeSupported {
			return nil, errors.Errorf(
				"Backup %s is a dependency of the following backup(s): %s; skipping. Use delete-backup --cascade to delete this chain",
				timestamp, strings.Join(names, ", "))
		}
		return nil, errors.Errorf(
			"Backup %s is a dependency of the following backup(s): %s. Use --cascade to delete them as well.",
			timestamp, strings.Join(names, ", "))
	}

	sort.Slice(dependents, func(i, j int) bool {
		return dependents[i].Timestamp > dependents[j].Timestamp
	})

	return append(dependents, target), nil
}

// validatePluginConfigMatch errors if any backup's Plugin field disagrees with whether
// --plugin-config was given for this invocation.
func validatePluginConfigMatch(backups []*history.BackupConfig, pluginConfigPath string) error {
	requestedPlugin := pluginConfigPath != ""
	for _, bc := range backups {
		usedPlugin := bc.Plugin != ""
		if usedPlugin == requestedPlugin {
			continue
		}
		if usedPlugin {
			return errors.Errorf(
				"Backup %s was created with plugin %s; pass --plugin-config to delete its data",
				bc.Timestamp, bc.Plugin)
		}
		return errors.Errorf(
			"Backup %s was not created with a plugin; drop --plugin-config to delete it",
			bc.Timestamp)
	}
	return nil
}

// newlyAddedDependents returns timestamps present in recheck but absent from original.
func newlyAddedDependents(original, recheck []*history.BackupConfig) []string {
	expected := make(map[string]bool, len(original))
	for _, bc := range original {
		expected[bc.Timestamp] = true
	}
	added := make([]string, 0)
	for _, bc := range recheck {
		if !expected[bc.Timestamp] {
			added = append(added, bc.Timestamp)
		}
	}
	return added
}

// promptForDeletion asks the user to confirm deleting backups. reader must be shared across
// every prompt in a run (rather than a fresh bufio.NewReader(os.Stdin) per call): bufio
// over-reads and buffers internally, so a piped multi-line answer like "y\ny\ny\n" would have its
// unread lines discarded along with the reader after the first prompt.
func promptForDeletion(reader *bufio.Reader, backups []*history.BackupConfig) bool {
	fmt.Println("The following backup(s) will be deleted:")
	for _, b := range backups {
		fmt.Printf("  %s (database: %s, date: %s)\n", b.Timestamp, b.DatabaseName, formatHistoryTimestamp(b.Timestamp))
	}
	fmt.Print("Continue? [y/N]: ")

	response, _ := reader.ReadString('\n')
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes"
}

func deleteOneBackup(historyDB *sql.DB, bc *history.BackupConfig, pluginConfig *utils.PluginConfig, segCluster *cluster.Cluster, coordinatorDataDir string) {
	gplog.Info("Deleting backup %s", bc.Timestamp)
	gplog.FatalOnError(history.SetDateDeleted(historyDB, bc.Timestamp, deleteStatusInProgress))

	if pluginConfig != nil {
		if err := pluginConfig.DeleteBackup(bc.Timestamp); err != nil {
			_ = history.SetDateDeleted(historyDB, bc.Timestamp, deleteStatusPluginFailed)
			gplog.FatalOnError(err)
		}
		// DeleteBackup only cleans the plugin's remote storage; local metadata/report files remain.
		if err := deleteLocalCoordinatorFiles(coordinatorDataDir, bc); err != nil {
			_ = history.SetDateDeleted(historyDB, bc.Timestamp, deleteStatusLocalFailed)
			gplog.FatalOnError(err)
		}
	} else {
		if err := deleteLocalBackupFiles(segCluster, bc); err != nil {
			_ = history.SetDateDeleted(historyDB, bc.Timestamp, deleteStatusLocalFailed)
			gplog.FatalOnError(err)
		}
	}

	gplog.FatalOnError(history.SetDateDeleted(historyDB, bc.Timestamp, history.CurrentTimestamp()))
	gplog.Info("Backup %s deleted", bc.Timestamp)
}

// deleteLocalBackupFiles removes the on-disk backup directory for bc on every primary and
// mirror host in the cluster (including the coordinator itself, content -1).
//
// This deliberately does not use cluster.GenerateAndExecuteCommand with a func(contentID int)
// string generator: that generator type addresses each content's primary host only, so a
// mirror's directory would get "rm -rf"'d over SSH to the wrong host. Instead this builds one
// ShellCommand per actual segment row (both "p" and "m") targeting that row's own host.
func deleteLocalBackupFiles(segCluster *cluster.Cluster, bc *history.BackupConfig) error {
	segPrefix, err := backupSegPrefix(bc)
	if err != nil {
		return err
	}

	primaryFPInfo := filepath.NewFilePathInfo(segCluster, bc.BackupDir, bc.Timestamp, segPrefix, bc.SingleBackupDir)
	mirrorFPInfo := filepath.NewFilePathInfo(segCluster, bc.BackupDir, bc.Timestamp, segPrefix, bc.SingleBackupDir, true)
	localHost := segCluster.GetHostForContent(-1, "p")

	commands := make([]cluster.ShellCommand, 0, len(segCluster.Segments))
	seenDirs := make(map[string]bool)
	for _, seg := range segCluster.Segments {
		fpInfo := primaryFPInfo
		if seg.Role == "m" {
			fpInfo = mirrorFPInfo
		}
		dir := fpInfo.GetDirForContent(seg.ContentID)
		if !path.IsAbs(dir) {
			return errors.Errorf(
				"Cannot determine the on-disk backup location for content %d on %s (got %q); refusing to delete a relative path",
				seg.ContentID, seg.Hostname, dir)
		}

		// SingleBackupDir maps every content on a host to the same dir; dedupe to avoid a race.
		key := seg.Hostname + "|" + dir
		if seenDirs[key] {
			continue
		}
		seenDirs[key] = true

		rmCommand := fmt.Sprintf("rm -rf %s", shellQuote(dir))
		useLocal := seg.Hostname == localHost
		commands = append(commands, cluster.NewShellCommand(cluster.ON_SEGMENTS, seg.ContentID, "",
			cluster.ConstructSSHCommand(useLocal, seg.Hostname, rmCommand)))
	}

	remoteOutput := segCluster.ExecuteClusterCommand(cluster.ON_SEGMENTS, commands)
	if remoteOutput.NumErrors > 0 {
		return errors.Errorf("Failed to delete backup files for %s on %d host(s)", bc.Timestamp, remoteOutput.NumErrors)
	}
	return nil
}

// coordinatorFPInfo builds the FilePathInfo needed to locate bc's own files on the coordinator's
// local disk (content -1 only), shared by every command that needs local backup paths without a
// full cluster topology.
func coordinatorFPInfo(coordinatorDataDir string, bc *history.BackupConfig) (filepath.FilePathInfo, error) {
	segPrefix, err := backupSegPrefix(bc)
	if err != nil {
		return filepath.FilePathInfo{}, err
	}

	return filepath.FilePathInfo{
		SegDirMap:              map[int]string{-1: coordinatorDataDir},
		Timestamp:              bc.Timestamp,
		UserSpecifiedBackupDir: bc.BackupDir,
		UserSpecifiedSegPrefix: segPrefix,
		SingleBackupDir:        bc.SingleBackupDir,
	}, nil
}

// deleteLocalCoordinatorFiles removes the coordinator's own local backup directory for bc.
// Runs directly on this host (no ssh) since delete-backup always executes on the coordinator.
func deleteLocalCoordinatorFiles(coordinatorDataDir string, bc *history.BackupConfig) error {
	fpInfo, err := coordinatorFPInfo(coordinatorDataDir, bc)
	if err != nil {
		return err
	}
	dir := fpInfo.GetDirForContent(-1)
	if !path.IsAbs(dir) {
		return errors.Errorf(
			"Cannot determine the coordinator's on-disk backup location for %s (got %q); refusing to delete a relative path",
			bc.Timestamp, dir)
	}

	if err := operating.System.RemoveAll(dir); err != nil {
		return errors.Errorf("Failed to delete local coordinator files for %s: %s", bc.Timestamp, err.Error())
	}
	return nil
}

// Unlike DoTeardown, there is no report/lock file/history entry to finalize here, only crash
// recovery.
func DoDeleteBackupTeardown() {
	if err := recover(); err != nil {
		if gplog.GetErrorCode() == 2 {
			// gplog.FatalOnError already logged to the log file, but not to the terminal.
			fmt.Println(err)
		} else {
			gplog.Error("%v", err)
			gplog.SetErrorCode(1)
		}
		os.Exit(gplog.GetErrorCode())
	}
}
