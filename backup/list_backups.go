package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/spf13/cobra"
	"github.com/warehouse-pg/common-go-libs/gplog"
	"github.com/warehouse-pg/common-go-libs/operating"
)

// This function handles setup for the list-backups subcommand and must run before flag parsing.
func DoListBackupsInit(cmd *cobra.Command) {
	RegisterListBackupsFlags(cmd.Flags())
}

func DoListBackups() {
	SetLoggerVerbosity()

	format := strings.ToLower(MustGetFlagString(options.FORMAT))
	if format != "text" && format != "json" {
		gplog.FatalOnError(fmt.Errorf("invalid --format value '%s', must be 'text' or 'json'", format))
	}

	coordinatorDataDir, err := getCoordinatorDataDir()
	gplog.FatalOnError(err)

	// GetBackupHistoryDatabasePath only reads SegDirMap[-1], so there is no need to query the
	// cluster for a full FilePathInfo; this lets list-backups work without a live DB connection.
	fpInfo := filepath.FilePathInfo{SegDirMap: map[int]string{-1: coordinatorDataDir}}
	historyDBPath := fpInfo.GetBackupHistoryDatabasePath()

	if _, err := os.Stat(historyDBPath); os.IsNotExist(err) {
		if format == "json" {
			fmt.Println("[]")
		} else {
			gplog.Info("No backup history database found at %s", historyDBPath)
		}
		return
	}

	historyDB, err := history.InitializeHistoryDatabase(historyDBPath)
	gplog.FatalOnError(err)
	defer historyDB.Close()

	backups, err := history.ListBackups(historyDB)
	gplog.FatalOnError(err)

	if !MustGetFlagBool(options.SHOW_ALL) {
		backups = filterOutDeletedBackups(backups)
	}

	if len(backups) == 0 {
		if format == "json" {
			fmt.Println("[]")
		} else {
			gplog.Info("No backups found in %s", historyDBPath)
		}
		return
	}

	if format == "json" {
		printBackupsListJSON(backups, fpInfo)
	} else {
		printBackupsList(backups, fpInfo)
	}
}

// configuredFPInfo returns a copy of fpInfo populated with b's timestamp, backup-dir, and
// segment-prefix fields. BackupConfig.BackupDir only records a --backup-dir override; a backup
// taken without that flag leaves it empty and lives under the coordinator data dir instead, so
// this falls back to the same default-location formula filepath.FilePathInfo already uses. A
// --backup-dir backup without --single-backup-dir stores its segment directories under a prefix
// (e.g. "gpseg") that isn't recorded in history; discover it from disk, best-effort, so the
// resolved path matches the real on-disk location.
func configuredFPInfo(fpInfo filepath.FilePathInfo, b history.BackupConfig) filepath.FilePathInfo {
	fpInfo.Timestamp = b.Timestamp
	fpInfo.UserSpecifiedBackupDir = b.BackupDir
	fpInfo.SingleBackupDir = b.SingleBackupDir
	if b.BackupDir != "" && !b.SingleBackupDir {
		if segPrefix, _, err := filepath.ParseSegPrefix(b.BackupDir, b.Timestamp); err == nil {
			fpInfo.UserSpecifiedSegPrefix = segPrefix
		}
	}
	return fpInfo
}

// actualBackupDir resolves the on-disk backup directory for b.
func actualBackupDir(fpInfo filepath.FilePathInfo, b history.BackupConfig) string {
	configured := configuredFPInfo(fpInfo, b)
	return configured.GetDirForContent(-1)
}

func filterOutDeletedBackups(backups []history.BackupConfig) []history.BackupConfig {
	filtered := make([]history.BackupConfig, 0, len(backups))
	for _, b := range backups {
		if !isFullyDeleted(b.DateDeleted) {
			filtered = append(filtered, b)
		}
	}
	return filtered
}

func printBackupsList(backups []history.BackupConfig, fpInfo filepath.FilePathInfo) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	defer w.Flush()

	fmt.Fprintln(w, "timestamp\tdate\tstatus\tdatabase\ttype\tobject filtering\tplugin\tduration\tdate deleted\tbackup dir\tcompressed\tcompression type")
	for _, b := range backups {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%t\t%s\n",
			b.Timestamp, formatHistoryTimestamp(b.Timestamp), b.Status, b.DatabaseName,
			backupTypeString(b), objectFilteringString(b), b.Plugin,
			backupDuration(b.Timestamp, b.EndTime), formatHistoryTimestamp(b.DateDeleted),
			actualBackupDir(fpInfo, b), b.Compressed, b.CompressionType)
	}
}

type backupListEntry struct {
	Timestamp       string `json:"timestamp"`
	Date            string `json:"date"`
	Status          string `json:"status"`
	Database        string `json:"database"`
	Type            string `json:"type"`
	ObjectFiltering string `json:"object_filtering"`
	Plugin          string `json:"plugin"`
	Duration        string `json:"duration"`
	DateDeleted     string `json:"date_deleted"`
	BackupDir       string `json:"backup_dir"`
	Compressed      bool   `json:"compressed"`
	CompressionType string `json:"compression_type"`
}

func printBackupsListJSON(backups []history.BackupConfig, fpInfo filepath.FilePathInfo) {
	entries := make([]backupListEntry, 0, len(backups))
	for _, b := range backups {
		entries = append(entries, backupListEntry{
			Timestamp:       b.Timestamp,
			Date:            formatHistoryTimestamp(b.Timestamp),
			Status:          b.Status,
			Database:        b.DatabaseName,
			Type:            backupTypeString(b),
			ObjectFiltering: objectFilteringString(b),
			Plugin:          b.Plugin,
			Duration:        backupDuration(b.Timestamp, b.EndTime),
			DateDeleted:     formatHistoryTimestamp(b.DateDeleted),
			BackupDir:       actualBackupDir(fpInfo, b),
			Compressed:      b.Compressed,
			CompressionType: b.CompressionType,
		})
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	gplog.FatalOnError(encoder.Encode(entries))
}

func backupDuration(startTimestamp string, endTimestamp string) string {
	start, err := time.ParseInLocation("20060102150405", startTimestamp, operating.System.Local)
	if err != nil {
		return ""
	}
	end, err := time.ParseInLocation("20060102150405", endTimestamp, operating.System.Local)
	if err != nil {
		return ""
	}

	elapsed := end.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	totalSeconds := int64(elapsed.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}

func backupTypeString(b history.BackupConfig) string {
	switch {
	case b.Incremental:
		return "incremental"
	case b.DataOnly:
		return "data-only"
	case b.MetadataOnly:
		return "metadata-only"
	default:
		return "full"
	}
}

func objectFilteringString(b history.BackupConfig) string {
	filters := make([]string, 0, 4)
	if b.IncludeSchemaFiltered {
		filters = append(filters, options.INCLUDE_SCHEMA)
	}
	if b.ExcludeSchemaFiltered {
		filters = append(filters, options.EXCLUDE_SCHEMA)
	}
	if b.IncludeTableFiltered {
		filters = append(filters, options.INCLUDE_RELATION)
	}
	if b.ExcludeTableFiltered {
		filters = append(filters, options.EXCLUDE_RELATION)
	}
	return strings.Join(filters, ", ")
}

// This function handles cleanup for the list-backups subcommand. Unlike DoTeardown, there is
// no backup report/lock file/history entry to finalize here, only crash recovery.
func DoListBackupsTeardown() {
	if err := recover(); err != nil {
		if gplog.GetErrorCode() == 2 {
			// gplog.FatalOnError already logged to the log file, but not to the terminal.
			fmt.Fprintln(os.Stderr, err)
		} else {
			gplog.Error("%v", err)
			gplog.SetErrorCode(1)
		}
		os.Exit(gplog.GetErrorCode())
	}
}
