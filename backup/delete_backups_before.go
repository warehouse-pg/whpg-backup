package backup

import (
	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/warehouse-pg/common-go-libs/gplog"
)

// Must run before flag parsing.
func DoDeleteBackupsBeforeInit(cmd *cobra.Command) {
	RegisterDeleteBackupsBeforeFlags(cmd.Flags())
}

// DoDeleteBackupsBefore deletes every backup older than cutoff by calling the same per-backup
// pipeline as delete-backup (deleteBackupChain) once per candidate timestamp. There is no
// --cascade here: it would let deletion reach past the cutoff into newer dependents. A backup
// that can't be deleted right now (still in progress, or blocked by a live dependent) is logged
// as a warning and skipped rather than aborting the rest of the run; a dependent skipped this way
// gets swept up on a later run once it's old enough to be a candidate itself.
func DoDeleteBackupsBefore(cutoff string) {
	SetLoggerVerbosity()

	if !filepath.IsValidTimestamp(cutoff) {
		gplog.Fatal(errors.Errorf("Invalid timestamp: %s", cutoff), "")
	}

	coordinatorDataDir, err := getCoordinatorDataDir()
	gplog.FatalOnError(err)

	historyDB, err := openBackupHistoryDatabase(coordinatorDataDir)
	gplog.FatalOnError(err)
	defer historyDB.Close()

	timestamps, err := history.GetBackupTimestampsBefore(historyDB, cutoff)
	gplog.FatalOnError(err)
	if len(timestamps) == 0 {
		gplog.Info("No backups found older than %s", formatHistoryTimestamp(cutoff))
		return
	}

	pluginConfigPath := MustGetFlagString(options.PLUGIN_CONFIG)
	pluginConfig, segCluster := setupDeletionTargets(coordinatorDataDir, pluginConfigPath)
	opts := deleteChainOptions{
		noPrompt:           MustGetFlagBool(options.NO_PROMPT),
		pluginConfigPath:   pluginConfigPath,
		pluginConfig:       pluginConfig,
		segCluster:         segCluster,
		coordinatorDataDir: coordinatorDataDir,
	}

	totalDeleted := 0
	failures := 0
	for _, ts := range timestamps {
		deleted, err := deleteBackupChain(historyDB, ts, opts)
		if err != nil {
			gplog.Warn("Skipping backup %s: %s", ts, err.Error())
			failures++
			continue
		}
		totalDeleted += deleted
	}

	gplog.Info("Successfully deleted %d backup(s) older than %s", totalDeleted, formatHistoryTimestamp(cutoff))
	if failures > 0 {
		gplog.Info("%d backup(s) could not be deleted; see warnings above", failures)
	}
}
