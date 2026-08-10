package backup

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/utils"
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

func formatHistoryTimestamp(ts string) string {
	t, err := time.ParseInLocation("20060102150405", ts, operating.System.Local)
	if err != nil {
		return ts
	}
	return t.Format("Mon Jan 2 2006 15:04:05")
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

	// GetBackupHistoryDatabasePath only reads SegDirMap[-1], so there is no need to query the
	// cluster for a full FilePathInfo to just locate the history db.
	fpInfo := filepath.FilePathInfo{SegDirMap: map[int]string{-1: coordinatorDataDir}}
	historyDBPath := fpInfo.GetBackupHistoryDatabasePath()
	if _, err := os.Stat(historyDBPath); os.IsNotExist(err) {
		gplog.Fatal(errors.Errorf("No backup history database found at %s", historyDBPath), "")
	}

	historyDB, err := history.InitializeHistoryDatabase(historyDBPath)
	gplog.FatalOnError(err)
	defer historyDB.Close()

	target, err := history.GetBackupConfig(timestamp, historyDB)
	gplog.FatalOnError(err)
	if isFullyDeleted(target.DateDeleted) {
		gplog.Info("Backup %s has already been deleted", timestamp)
		return
	}

	cascade := MustGetFlagBool(options.CASCADE)
	deletionOrder, err := resolveDeletionOrder(historyDB, timestamp, cascade)
	gplog.FatalOnError(err)

	backupsToDelete := make([]*history.BackupConfig, 0, len(deletionOrder))
	for _, ts := range deletionOrder {
		bc, err := history.GetBackupConfig(ts, historyDB)
		gplog.FatalOnError(err)
		backupsToDelete = append(backupsToDelete, bc)
	}

	if !MustGetFlagBool(options.NO_PROMPT) && !promptForDeletion(backupsToDelete) {
		gplog.Info("Backup deletion cancelled")
		return
	}

	var pluginConfig *utils.PluginConfig
	if pluginConfigPath := MustGetFlagString(options.PLUGIN_CONFIG); pluginConfigPath != "" {
		pluginConfig, err = utils.ReadPluginConfig(pluginConfigPath)
		gplog.FatalOnError(err)
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

	for _, bc := range backupsToDelete {
		deleteOneBackup(historyDB, bc, pluginConfig, segCluster)
	}
	gplog.Info("Successfully deleted %d backup(s)", len(backupsToDelete))
}

// Returns dependents (transitive, not-yet-deleted) before timestamp itself, or an error if
// dependents exist and cascade is false.
func resolveDeletionOrder(historyDB *sql.DB, timestamp string, cascade bool) ([]string, error) {
	visited := map[string]bool{timestamp: true}
	dependents := make([]string, 0)
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
			visited[dep] = true
			dependents = append(dependents, dep)
			queue = append(queue, dep)
		}
	}

	if len(dependents) > 0 && !cascade {
		return nil, errors.Errorf(
			"Backup %s is a dependency of the following backup(s): %s. Use --cascade to delete them as well.",
			timestamp, strings.Join(dependents, ", "))
	}

	return append(dependents, timestamp), nil
}

func promptForDeletion(backups []*history.BackupConfig) bool {
	fmt.Println("The following backup(s) will be deleted:")
	for _, b := range backups {
		fmt.Printf("  %s (database: %s, date: %s)\n", b.Timestamp, b.DatabaseName, formatHistoryTimestamp(b.Timestamp))
	}
	fmt.Print("Continue? [y/N]: ")

	reader := bufio.NewReader(os.Stdin)
	response, _ := reader.ReadString('\n')
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes"
}

func deleteOneBackup(historyDB *sql.DB, bc *history.BackupConfig, pluginConfig *utils.PluginConfig, segCluster *cluster.Cluster) {
	gplog.Info("Deleting backup %s", bc.Timestamp)
	gplog.FatalOnError(history.SetDateDeleted(historyDB, bc.Timestamp, deleteStatusInProgress))

	if pluginConfig != nil {
		if err := pluginConfig.DeleteBackup(bc.Timestamp); err != nil {
			_ = history.SetDateDeleted(historyDB, bc.Timestamp, deleteStatusPluginFailed)
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
// mirror host in the cluster.
//
// This deliberately does not use cluster.GenerateAndExecuteCommand with a func(contentID int)
// string generator: that generator type addresses each content's primary host only, so a
// mirror's directory would get "rm -rf"'d over SSH to the wrong host. Instead this builds one
// ShellCommand per actual segment row (both "p" and "m") targeting that row's own host.
func deleteLocalBackupFiles(segCluster *cluster.Cluster, bc *history.BackupConfig) error {
	// UserSpecifiedSegPrefix is left empty: it only supports restoring the legacy backup file
	// format, and new backups never set it (see filepath.NewFilePathInfo).
	primaryFPInfo := filepath.NewFilePathInfo(segCluster, bc.BackupDir, bc.Timestamp, "", bc.SingleBackupDir)
	mirrorFPInfo := filepath.NewFilePathInfo(segCluster, bc.BackupDir, bc.Timestamp, "", bc.SingleBackupDir, true)
	localHost := segCluster.GetHostForContent(-1, "p")

	commands := make([]cluster.ShellCommand, 0, len(segCluster.Segments))
	for _, seg := range segCluster.Segments {
		fpInfo := primaryFPInfo
		if seg.Role == "m" {
			fpInfo = mirrorFPInfo
		}
		dir := fpInfo.GetDirForContent(seg.ContentID)
		if dir == "" {
			// No mirror configured for this content.
			continue
		}
		rmCommand := fmt.Sprintf("rm -rf %s", dir)
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
