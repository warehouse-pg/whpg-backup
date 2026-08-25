package backup

import (
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/warehouse-pg/common-go-libs/gplog"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v2"
)

// findTableScanConcurrency bounds how many backups are inspected in parallel by
// findBackupsContainingTable. Each unit of work is I/O-bound (a history DB query plus a TOC file
// read), so this is sized like an I/O worker pool rather than tied to available CPUs, but it's
// still capped relative to NumCPU to avoid overwhelming installations with very large histories.
var findTableScanConcurrency = runtime.NumCPU() * 4

// Must run before flag parsing.
func DoFindTableInit(cmd *cobra.Command) {
	RegisterFindTableFlags(cmd.Flags())
}

// DoFindTable prints every successful, not-yet-deleted backup whose table-of-contents shows it
// backed up data for the given table. Metadata-only backups never have table data, so they are
// never candidates.
func DoFindTable(tableFQN string) {
	SetLoggerVerbosity()

	format := strings.ToLower(MustGetFlagString(options.FORMAT))
	if format != "text" && format != "json" {
		gplog.FatalOnError(fmt.Errorf("invalid --format value '%s', must be 'text' or 'json'", format))
	}

	schema, table, err := parseTableFQN(tableFQN)
	gplog.FatalOnError(err)

	coordinatorDataDir, err := getCoordinatorDataDir()
	gplog.FatalOnError(err)

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

	matches, warnings, err := findBackupsContainingTable(historyDB, coordinatorDataDir, schema, table)
	gplog.FatalOnError(err)

	// gplog.Warn writes to stdout, so route warnings to stderr in json mode to keep stdout
	// parseable.
	for _, w := range warnings {
		if format == "json" {
			fmt.Fprintln(os.Stderr, "WARNING: "+w)
		} else {
			gplog.Warn("%s", w)
		}
	}

	if len(matches) == 0 {
		if format == "json" {
			fmt.Println("[]")
		} else {
			gplog.Info("No backups found containing table %s.%s", schema, table)
		}
		return
	}

	backups := make([]history.BackupConfig, len(matches))
	for i, bc := range matches {
		backups[i] = *bc
	}

	if format == "json" {
		printBackupsListJSON(backups)
	} else {
		printBackupsList(backups)
	}
}

// findBackupsContainingTable scans every stored backup and returns the BackupConfigs of those
// that are successful, not deleted, not metadata-only, don't have a failed delete attempt against
// them, and whose table-of-contents lists schema.table among its data entries, newest first
// (matching list-backups' order). Backups are inspected concurrently (bounded
// by findTableScanConcurrency), since each check is dominated by I/O (a history DB query plus a
// TOC file read) rather than CPU. A backup whose history entry or TOC can't be read (e.g. a
// malformed row, or local files removed by something other than delete-backup) is skipped, with
// the reason returned as a warning instead of logged, leaving the sink to the caller.
func findBackupsContainingTable(historyDB *sql.DB, coordinatorDataDir string, schema string, table string) ([]*history.BackupConfig, []string, error) {
	timestamps, err := history.GetAllBackupTimestamps(historyDB)
	if err != nil {
		return nil, nil, err
	}

	results := make([]*history.BackupConfig, len(timestamps))
	warnings := make([]string, len(timestamps))

	group := new(errgroup.Group)
	group.SetLimit(findTableScanConcurrency)
	for i, ts := range timestamps {
		i, ts := i, ts
		group.Go(func() error {
			bc, err := history.GetBackupConfig(ts, historyDB)
			if err != nil {
				warnings[i] = fmt.Sprintf("Skipping backup %s: could not read its history entry: %s", ts, err.Error())
				return nil
			}
			if bc.Status != history.BackupStatusSucceed || bc.MetadataOnly ||
				isFullyDeleted(bc.DateDeleted) || isDeleteFailed(bc.DateDeleted) {
				return nil
			}

			fpInfo, err := coordinatorFPInfo(coordinatorDataDir, bc)
			if err != nil {
				warnings[i] = fmt.Sprintf("Skipping backup %s: %s", ts, err.Error())
				return nil
			}

			tocFile, err := readTOC(fpInfo.GetTOCFilePath())
			if err != nil {
				warnings[i] = fmt.Sprintf("Skipping backup %s: could not read its table of contents: %s", ts, err.Error())
				return nil
			}

			if tocContainsTable(tocFile, schema, table) {
				results[i] = bc
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}

	nonEmptyWarnings := make([]string, 0)
	for _, w := range warnings {
		if w != "" {
			nonEmptyWarnings = append(nonEmptyWarnings, w)
		}
	}

	// Walk results back-to-front so matches come out newest first, matching list-backups' order.
	matches := make([]*history.BackupConfig, 0)
	for i := len(results) - 1; i >= 0; i-- {
		if results[i] != nil {
			matches = append(matches, results[i])
		}
	}

	return matches, nonEmptyWarnings, nil
}

// readTOC is toc.NewTOC's non-fatal counterpart: find-table scans every backup ever taken, so
// one missing/unreadable file should be skipped with a warning rather than aborting the whole
// scan, unlike a single-backup operation like restore where a bad TOC really is fatal.
func readTOC(tocPath string) (*toc.TOC, error) {
	contents, err := os.ReadFile(tocPath)
	if err != nil {
		return nil, err
	}
	tocFile := &toc.TOC{}
	if err := yaml.Unmarshal(contents, tocFile); err != nil {
		return nil, err
	}
	return tocFile, nil
}

// tocContainsTable reports whether tocFile backed up data for schema.table, either directly or,
// for a partitioned table, as the root of at least one backed-up leaf partition (leaf partitions
// are the only ones that get their own data entries; matching a root by name is how
// --include-table finds them too, see toc.GetIncludedPartitionRoots).
//
// TOC entries store schema/table/partition-root names exactly as quote_ident() returned them at
// backup time (see the quote_ident calls backing table.Schema/table.Name in
// queries_relations.go), so a name only carries surrounding quotes when Postgres judged them
// necessary (mixed case, reserved word, embedded dot, etc). schema/table here are always the bare,
// unquoted identifier value (parseTableFQN's job), so entries must be unquoted before comparing
// rather than the other way around - that avoids having to reimplement quote_ident's own quoting
// rules just to decide when to re-add quotes.
func tocContainsTable(tocFile *toc.TOC, schema string, table string) bool {
	for _, entry := range tocFile.DataEntries {
		if utils.UnquoteIdent(entry.Schema) == schema &&
			(utils.UnquoteIdent(entry.Name) == table || utils.UnquoteIdent(entry.PartitionRoot) == table) {
			return true
		}
	}
	return false
}

// parseTableFQN splits a "schema.table" argument the same way gpbackup/gprestore's
// --include-table option does: a quoted identifier is taken verbatim (only doubled quotes are
// unescaped) and may itself contain a literal dot, while a bare identifier is folded to lowercase
// to match Postgres's own folding of unquoted identifiers. This can't reuse
// options.SeparateSchemaAndTable + QuoteTableNames wholesale, since that folding normally happens
// via a quote_ident() round trip against a live connection, and find-table (like delete-backup)
// only has the history database and local files to work from, never a database connection. It
// does reuse options.SplitFQN for the quote-aware splitting itself.
func parseTableFQN(fqn string) (schema string, table string, err error) {
	schema, table, schemaQuoted, tableQuoted, err := options.SplitFQN(fqn)
	if err != nil {
		return "", "", errors.Errorf(`Table "%s" is not correctly fully-qualified.  Please ensure table is in the format "schema.table".`, fqn)
	}

	return foldIdentifier(schema, schemaQuoted), foldIdentifier(table, tableQuoted), nil
}

// foldIdentifier mirrors how Postgres itself resolves an identifier: verbatim (with doubled
// quotes collapsed to one) if it was quoted, lowercased otherwise.
func foldIdentifier(part string, wasQuoted bool) string {
	if !wasQuoted {
		return strings.ToLower(part)
	}
	return strings.ReplaceAll(part, `""`, `"`)
}
