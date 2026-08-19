package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/warehouse-pg/common-go-libs/gplog"
)

// This function handles setup for the display-report subcommand and must run before flag parsing.
func DoDisplayReportInit(cmd *cobra.Command) {
	RegisterDisplayReportFlags(cmd.Flags())
}

func DoDisplayReport(timestamp string) {
	SetLoggerVerbosity()

	if !filepath.IsValidTimestamp(timestamp) {
		gplog.Fatal(errors.Errorf("Invalid timestamp: %s", timestamp), "")
	}

	format := strings.ToLower(MustGetFlagString(options.FORMAT))
	if format != "text" && format != "json" {
		gplog.FatalOnError(fmt.Errorf("invalid --format value '%s', must be 'text' or 'json'", format))
	}

	coordinatorDataDir, err := getCoordinatorDataDir()
	gplog.FatalOnError(err)

	historyDB, err := openBackupHistoryDatabase(coordinatorDataDir)
	gplog.FatalOnError(err)
	defer historyDB.Close()

	bc, err := history.GetBackupConfig(timestamp, historyDB)
	if err != nil {
		gplog.Fatal(errors.Errorf("Backup %s not found in history database", timestamp), "")
	}

	fpInfo := filepath.FilePathInfo{SegDirMap: map[int]string{-1: coordinatorDataDir}}
	configured := configuredFPInfo(fpInfo, *bc)
	reportPath := configured.GetBackupReportFilePath()

	if _, statErr := os.Stat(reportPath); statErr != nil {
		if !os.IsNotExist(statErr) {
			gplog.FatalOnError(statErr)
		}
		fetchPluginReportFile(bc, reportPath)
	}

	contents, err := os.ReadFile(reportPath)
	gplog.FatalOnError(err)

	if format == "json" {
		printReportJSON(*bc, reportPath, string(contents))
	} else {
		fmt.Print(string(contents))
	}
}

// fetchPluginReportFile pulls a plugin-backed backup's report file down to reportPath, since a
// plugin backup's local copy may have been cleaned up after upload. Fatal if the backup used a
// plugin but no --plugin-config was given to locate it, or if the backup used no plugin at all
// and the local file is simply missing.
func fetchPluginReportFile(bc *history.BackupConfig, reportPath string) {
	if bc.Plugin == "" {
		gplog.Fatal(errors.Errorf("Backup report file not found at %s", reportPath), "")
	}

	pluginConfigPath := MustGetFlagString(options.PLUGIN_CONFIG)
	if pluginConfigPath == "" {
		gplog.Fatal(errors.Errorf(
			"Backup %s was created with plugin %s; pass --plugin-config to retrieve its report",
			bc.Timestamp, bc.Plugin), "")
	}

	pluginConfig, err := utils.ReadPluginConfig(pluginConfigPath)
	gplog.FatalOnError(err)
	pluginConfig.ConfigPath = pluginConfigPath
	pluginConfig.MustRestoreFile(reportPath)
}

func printReportJSON(bc history.BackupConfig, reportPath string, reportText string) {
	fields, objectCounts := parseReportText(reportText)

	entry := make(map[string]interface{}, len(fields)+5)
	for k, v := range fields {
		entry[k] = v
	}
	entry["timestamp"] = bc.Timestamp
	entry["database"] = bc.DatabaseName
	entry["status"] = bc.Status
	entry["report_file"] = reportPath
	entry["object_counts"] = objectCounts

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	gplog.FatalOnError(encoder.Encode(entry))
}

// parseReportText splits a gpbackup report file's text into its "key: value" header fields and
// its trailing "count of database objects in backup" section, keyed as snake_case for JSON. A
// few header values (e.g. the incremental backup set's timestamps, or a multi-line backup error)
// span multiple physical lines with no colon of their own; such a line is folded into the most
// recently seen key. A blank line resets that tracking so an unrelated later line never attaches
// to a stale key.
func parseReportText(text string) (map[string]string, map[string]int) {
	fields := make(map[string]string)
	objectCounts := make(map[string]int)

	inObjectCounts := false
	lastKey := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			lastKey = ""
			continue
		}
		if strings.EqualFold(trimmed, "count of database objects in backup:") {
			inObjectCounts = true
			continue
		}

		if inObjectCounts {
			parts := strings.Fields(trimmed)
			if len(parts) < 2 {
				continue
			}
			count, err := strconv.Atoi(parts[len(parts)-1])
			if err != nil {
				continue
			}
			key := strings.ReplaceAll(strings.Join(parts[:len(parts)-1], " "), " ", "_")
			objectCounts[key] = count
			continue
		}

		idx := strings.Index(trimmed, ":")
		if idx == -1 {
			if lastKey != "" {
				fields[lastKey] += "\n" + trimmed
			}
			continue
		}
		key := strings.ReplaceAll(strings.TrimSpace(trimmed[:idx]), " ", "_")
		value := strings.TrimSpace(trimmed[idx+1:])
		fields[key] = value
		lastKey = key
	}

	return fields, objectCounts
}

// This function handles cleanup for the display-report subcommand. Unlike DoTeardown, there is
// no backup report/lock file/history entry to finalize here, only crash recovery.
func DoDisplayReportTeardown() {
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
