package end_to_end_test

import (
	"fmt"
	"os"
	path "path/filepath"
	"regexp"
	"strconv"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/toc"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/testhelper"
	"gopkg.in/yaml.v2"
)

/*
 * DISTRIBUTED COORDINATOR ONLY tables (WHPG 7.4 and later) keep their whole
 * heap on the coordinator; every segment holds an empty relation.  COPY ...
 * ON SEGMENT therefore reads nothing on backup and writes to the wrong place
 * on restore -- the rows land physically on the segments, where no ordinary
 * query on such a table ever looks, so count(*) stays 0 while
 * gp_dist_random() shows the data.  Both directions used to succeed silently,
 * so these tests assert on row counts from both angles.
 */

const coordinatorOnlyRowCount = 25

// Heap, AO row and AO column, so the specs cover every storage layout a
// coordinator-only table can have.
var coordinatorOnlyTableNames = []string{"public.co_heap", "public.co_ao", "public.co_aoco"}

// The example plugin appends a line per call here; hardcoded in the plugin, the
// same way examplePluginTestDir is.
const examplePluginLogPath = "/tmp/plugin_out.txt"

// findCoordinatorOnlyDataFile returns the path of the per-table data file the
// backup should have written into the coordinator's own backup directory.
func findCoordinatorOnlyDataFile(backupDir string, timestamp string, oid uint32) []string {
	pattern := path.Join(backupDir, "backups", timestamp[:8], timestamp,
		fmt.Sprintf("gpbackup_-1_%s_%d*", timestamp, oid))
	matches, err := path.Glob(pattern)
	Expect(err).ToNot(HaveOccurred())
	if len(matches) == 0 {
		matches, err = path.Glob(path.Join(backupDir, "*-1", "backups", timestamp[:8], timestamp,
			fmt.Sprintf("gpbackup_-1_%s_%d*", timestamp, oid)))
		Expect(err).ToNot(HaveOccurred())
	}
	return matches
}

// patchConfigSegmentCount rewrites the segment count recorded in a backup's
// config so that --resize-cluster sees a cluster-size change without needing a
// second cluster to back up from.  gprestore downgrades --resize-cluster to a
// normal restore when the counts match, so the count has to differ for the
// resize code paths to run at all.
func patchConfigSegmentCount(backupDir string, timestamp string, segmentCount int) {
	pattern := path.Join(backupDir, "backups", timestamp[:8], timestamp,
		fmt.Sprintf("gpbackup_%s_config.yaml", timestamp))
	matches, err := path.Glob(pattern)
	Expect(err).ToNot(HaveOccurred())
	if len(matches) == 0 {
		matches, err = path.Glob(path.Join(backupDir, "*-1", "backups", timestamp[:8], timestamp,
			fmt.Sprintf("gpbackup_%s_config.yaml", timestamp)))
		Expect(err).ToNot(HaveOccurred())
	}
	Expect(matches).To(HaveLen(1))
	configPath := matches[0]

	cfg := history.ReadConfigFile(configPath)
	cfg.SegmentCount = segmentCount
	// gpbackup leaves the config file read-only (0444) and WriteConfigFile
	// swallows the resulting EACCES, so make it writable first.
	Expect(os.Chmod(configPath, 0644)).To(Succeed())
	history.WriteConfigFile(cfg, configPath)
}

func createCoordinatorOnlyTables(conn *dbconn.DBConn) {
	testhelper.AssertQueryRuns(conn,
		"CREATE TABLE public.co_heap(a int, b text) DISTRIBUTED COORDINATOR ONLY;")
	testhelper.AssertQueryRuns(conn,
		"CREATE TABLE public.co_ao(a int, b text) WITH (appendonly=true, orientation=row) DISTRIBUTED COORDINATOR ONLY;")
	testhelper.AssertQueryRuns(conn,
		"CREATE TABLE public.co_aoco(a int, b text) WITH (appendonly=true, orientation=column) DISTRIBUTED COORDINATOR ONLY;")
	for _, tableName := range coordinatorOnlyTableNames {
		testhelper.AssertQueryRuns(conn, fmt.Sprintf(
			"INSERT INTO %s SELECT i, format('row%%s', i) FROM generate_series(1, %d) i;",
			tableName, coordinatorOnlyRowCount))
	}
	for _, tableName := range coordinatorOnlyTableNames {
		testhelper.AssertQueryRuns(conn, fmt.Sprintf("ANALYZE %s;", tableName))
	}
}

// coordinatorOnlyBackupArgs returns the given gpbackup flags followed by the
// --include-table pairs that narrow the backup down to the fixture tables, so
// the specs do not depend on whatever else the shared testdb has accumulated.
func coordinatorOnlyBackupArgs(flags ...string) []string {
	args := make([]string, 0, len(flags)+2*len(coordinatorOnlyTableNames))
	args = append(args, flags...)
	for _, tableName := range coordinatorOnlyTableNames {
		args = append(args, "--include-table", tableName)
	}
	return args
}

func dropCoordinatorOnlyTables(conn *dbconn.DBConn) {
	for _, tableName := range coordinatorOnlyTableNames {
		conn.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s;", tableName))
	}
}

/*
 * assertCoordinatorOnlyDataRestored checks the restored data from both sides:
 * the rows must be visible to a normal query (so they are on the coordinator)
 * and gp_dist_random() must find nothing (so none of them were loaded onto a
 * segment, which is the silently-wrong state the old ON SEGMENT restore left).
 */
func assertCoordinatorOnlyDataRestored(conn *dbconn.DBConn, tableNames []string) {
	for _, tableName := range tableNames {
		Expect(dbconn.MustSelectString(conn, fmt.Sprintf(
			"SELECT count(*) AS string FROM %s", tableName))).
			To(Equal(strconv.Itoa(coordinatorOnlyRowCount)),
				fmt.Sprintf("wrong number of rows visible in %s", tableName))
		Expect(dbconn.MustSelectString(conn, fmt.Sprintf(
			"SELECT coalesce(sum(a), 0) AS string FROM %s", tableName))).
			To(Equal(strconv.Itoa(coordinatorOnlyRowCount*(coordinatorOnlyRowCount+1)/2)),
				fmt.Sprintf("wrong data restored into %s", tableName))
		Expect(dbconn.MustSelectString(conn, fmt.Sprintf(
			"SELECT count(*) AS string FROM gp_dist_random('%s')", tableName))).
			To(Equal("0"),
				fmt.Sprintf("rows were restored onto the segments for coordinator-only table %s", tableName))
	}
}

/*
 * assertCoordinatorOnlyRows is assertCoordinatorOnlyDataRestored for a single
 * table whose row count is not the fixture's, which an incremental restore
 * produces.  It still checks both sides: visible on the coordinator, absent
 * from the segments.
 */
func assertCoordinatorOnlyRows(conn *dbconn.DBConn, tableName string, expectedRows int) {
	Expect(dbconn.MustSelectString(conn, fmt.Sprintf(
		"SELECT count(*) AS string FROM %s", tableName))).
		To(Equal(strconv.Itoa(expectedRows)),
			fmt.Sprintf("wrong number of rows visible in %s", tableName))
	Expect(dbconn.MustSelectString(conn, fmt.Sprintf(
		"SELECT count(*) AS string FROM gp_dist_random('%s')", tableName))).
		To(Equal("0"),
			fmt.Sprintf("rows were restored onto the segments for coordinator-only table %s", tableName))
}

/*
 * dataEntryFQNsInTOC returns the tables whose data a given backup actually
 * carries.  An incremental backup holds data only for the tables it decided had
 * changed, so this is what says whether the modcount comparison noticed a
 * coordinator-only AO table's new rows.
 */
func dataEntryFQNsInTOC(backupDir string, timestamp string) []string {
	tocStruct := &toc.TOC{}
	Expect(yaml.Unmarshal(getMetdataFileContents(backupDir, timestamp, "toc.yaml"), tocStruct)).To(Succeed())

	fqns := make([]string, 0, len(tocStruct.DataEntries))
	for _, entry := range tocStruct.DataEntries {
		fqns = append(fqns, fmt.Sprintf("%s.%s", entry.Schema, entry.Name))
	}
	return fqns
}

/*
 * assertPluginBackedUpDataOnCoordinator reads the example plugin's own call log
 * to check where its backup_data hook ran from.
 *
 * A coordinator-only table's data is piped to the plugin by the COPY on the
 * coordinator, so the path it is handed carries content -1.  Segment data
 * reaches the plugin through gpbackup_helper instead, with that segment's
 * content in the path -- so for a backup set that is entirely coordinator-only
 * there must be one content -1 call per table and no per-segment call at all.
 * Backing the data up and restoring it does not on its own show which of the
 * two routes carried it.
 */
func assertPluginBackedUpDataOnCoordinator(timestamp string, numTables int) {
	contents, err := os.ReadFile(examplePluginLogPath)
	Expect(err).ToNot(HaveOccurred(), "the example plugin recorded no calls at all")

	coordinatorCalls := regexp.MustCompile(fmt.Sprintf(
		`(?m)^backup_data \S+ \S*gpbackup_-1_%s_[0-9]+`, timestamp)).
		FindAllString(string(contents), -1)
	Expect(coordinatorCalls).To(HaveLen(numTables),
		"expected the plugin's backup_data to be invoked once per coordinator-only table with a content -1 path")

	segmentCalls := regexp.MustCompile(fmt.Sprintf(
		`(?m)^backup_data \S+ \S*gpbackup_[0-9]+_%s_`, timestamp)).
		FindAllString(string(contents), -1)
	Expect(segmentCalls).To(BeEmpty(),
		"no segment should have sent data to the plugin for an entirely coordinator-only backup")

	// The upload has to have produced the files, not just made the calls.
	Expect(path.Glob(path.Join(examplePluginTestDir, timestamp[:8], timestamp,
		fmt.Sprintf("gpbackup_-1_%s_*", timestamp)))).
		To(HaveLen(numTables), "the plugin did not store the coordinator's data files")
}

// assertCoordinatorOnlyInTOC checks that the data entries for the given tables
// are flagged as coordinator-only, which is what drives the restore side.
func assertCoordinatorOnlyInTOC(backupDir string, timestamp string, tableNames []string) map[string]uint32 {
	tocStruct := &toc.TOC{}
	Expect(yaml.Unmarshal(getMetdataFileContents(backupDir, timestamp, "toc.yaml"), tocStruct)).To(Succeed())

	oids := make(map[string]uint32)
	for _, name := range tableNames {
		found := false
		for _, entry := range tocStruct.DataEntries {
			if fmt.Sprintf("%s.%s", entry.Schema, entry.Name) != name {
				continue
			}
			found = true
			Expect(entry.IsCoordinatorOnly).To(BeTrue(),
				fmt.Sprintf("%s was not recorded as coordinator-only in the TOC", name))
			Expect(entry.RowsCopied).To(Equal(int64(coordinatorOnlyRowCount)),
				fmt.Sprintf("%s was backed up with the wrong row count", name))
			oids[name] = entry.Oid
		}
		Expect(found).To(BeTrue(), fmt.Sprintf("%s is missing from the TOC data entries", name))
	}
	return oids
}

var _ = Describe("coordinator-only table end to end tests", func() {
	coordinatorOnlyTables := coordinatorOnlyTableNames

	BeforeEach(func() {
		end_to_end_setup()
		if backupConn.Version.Before("7.4") {
			Skip("DISTRIBUTED COORDINATOR ONLY tables require WHPG 7.4 or later")
		}
		dropCoordinatorOnlyTables(backupConn)
		dropCoordinatorOnlyTables(restoreConn)
		createCoordinatorOnlyTables(backupConn)
	})
	AfterEach(func() {
		dropCoordinatorOnlyTables(backupConn)
		dropCoordinatorOnlyTables(restoreConn)
		end_to_end_teardown()
	})

	It("backs up and restores coordinator-only heap and AO tables", func() {
		output := gpbackup(gpbackupPath, backupHelperPath,
			coordinatorOnlyBackupArgs("--backup-dir", backupDir)...)
		timestamp := getBackupTimestamp(string(output))
		Expect(timestamp).ToNot(BeEmpty())

		oids := assertCoordinatorOnlyInTOC(backupDir, timestamp, coordinatorOnlyTables)
		for name, oid := range oids {
			Expect(findCoordinatorOnlyDataFile(backupDir, timestamp, oid)).To(HaveLen(1),
				fmt.Sprintf("expected one data file under content -1 for %s", name))
		}

		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb",
			"--backup-dir", backupDir)

		assertCoordinatorOnlyDataRestored(restoreConn, coordinatorOnlyTables)
	})

	It("backs up and restores coordinator-only tables with --single-data-file", func() {
		// The coordinator has no gpbackup_helper, so a coordinator-only table
		// gets a file of its own even in single-data-file mode.  With no other
		// tables in the set there is no segment data at all, so the helpers
		// must not be started -- the backup agent would otherwise block
		// forever on a pipe that no COPY ever opens.
		output := gpbackup(gpbackupPath, backupHelperPath,
			coordinatorOnlyBackupArgs("--backup-dir", backupDir, "--single-data-file")...)
		timestamp := getBackupTimestamp(string(output))
		Expect(timestamp).ToNot(BeEmpty())

		assertCoordinatorOnlyInTOC(backupDir, timestamp, coordinatorOnlyTables)

		// gprestore takes no --single-data-file: it reads that from the
		// backup's own config.
		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb",
			"--backup-dir", backupDir)

		assertCoordinatorOnlyDataRestored(restoreConn, coordinatorOnlyTables)
	})

	It("backs up and restores coordinator-only and distributed tables together with --single-data-file", func() {
		testhelper.AssertQueryRuns(backupConn,
			"CREATE TABLE public.seg_t(a int, b text) DISTRIBUTED BY (a);")
		defer backupConn.Exec("DROP TABLE IF EXISTS public.seg_t;")
		defer restoreConn.Exec("DROP TABLE IF EXISTS public.seg_t;")
		testhelper.AssertQueryRuns(backupConn, fmt.Sprintf(
			"INSERT INTO public.seg_t SELECT i, format('row%%s', i) FROM generate_series(1, %d) i;",
			coordinatorOnlyRowCount))

		output := gpbackup(gpbackupPath, backupHelperPath,
			append(coordinatorOnlyBackupArgs("--backup-dir", backupDir, "--single-data-file"),
				"--include-table", "public.seg_t")...)
		timestamp := getBackupTimestamp(string(output))
		Expect(timestamp).ToNot(BeEmpty())

		// gprestore takes no --single-data-file: it reads that from the
		// backup's own config.
		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb",
			"--backup-dir", backupDir)

		assertCoordinatorOnlyDataRestored(restoreConn, coordinatorOnlyTables)
		assertDataRestored(restoreConn, map[string]int{"public.seg_t": coordinatorOnlyRowCount})
	})

	It("restores coordinator-only tables into a cluster with a different segment count", func() {
		// A coordinator-only table cannot be redistributed: the server rejects
		// both SET WITH (REORGANIZE=true) and EXPAND TABLE for it, so a resize
		// restore must leave its distribution alone.
		output := gpbackup(gpbackupPath, backupHelperPath,
			coordinatorOnlyBackupArgs("--backup-dir", backupDir)...)
		timestamp := getBackupTimestamp(string(output))
		Expect(timestamp).ToNot(BeEmpty())

		patchConfigSegmentCount(backupDir, timestamp, segmentCount+1)

		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb",
			"--backup-dir", backupDir,
			"--resize-cluster")

		assertCoordinatorOnlyDataRestored(restoreConn, coordinatorOnlyTables)
	})

	It("notices coordinator-side changes to AO tables in an incremental backup", func() {
		/*
		 * An AO table is only re-backed-up by an incremental if its modcount or
		 * DDL timestamp moved.  On 7 and later that modcount is read through
		 * gp_dist_random(), which is permanently empty for a coordinator-only
		 * table -- so it would read 0 forever, the table would look unchanged,
		 * and the incremental would silently carry none of its new rows.
		 */
		output := gpbackup(gpbackupPath, backupHelperPath,
			coordinatorOnlyBackupArgs("--backup-dir", backupDir, "--leaf-partition-data")...)
		fullTimestamp := getBackupTimestamp(string(output))
		Expect(fullTimestamp).ToNot(BeEmpty())

		// Change one AO table and leave the other alone, so this fails both if
		// the modcount never moves and if it reads as changed unconditionally.
		testhelper.AssertQueryRuns(backupConn, fmt.Sprintf(
			"INSERT INTO public.co_ao VALUES (%d, 'incremental');", coordinatorOnlyRowCount+1))

		output = gpbackup(gpbackupPath, backupHelperPath,
			coordinatorOnlyBackupArgs("--backup-dir", backupDir, "--leaf-partition-data",
				"--incremental", "--from-timestamp", fullTimestamp)...)
		incrementalTimestamp := getBackupTimestamp(string(output))
		Expect(incrementalTimestamp).ToNot(BeEmpty())

		changed := dataEntryFQNsInTOC(backupDir, incrementalTimestamp)
		Expect(changed).To(ContainElement("public.co_ao"),
			"the incremental backup did not notice the insert on the coordinator")
		Expect(changed).ToNot(ContainElement("public.co_aoco"),
			"the incremental backup re-backed-up an AO table that had not changed")

		gprestore(gprestorePath, restoreHelperPath, incrementalTimestamp,
			"--redirect-db", "restoredb",
			"--backup-dir", backupDir)

		assertCoordinatorOnlyRows(restoreConn, "public.co_ao", coordinatorOnlyRowCount+1)
		assertCoordinatorOnlyRows(restoreConn, "public.co_aoco", coordinatorOnlyRowCount)
		assertCoordinatorOnlyRows(restoreConn, "public.co_heap", coordinatorOnlyRowCount)
	})

	It("backs up and restores coordinator-only tables through a plugin", func() {
		copyPluginToAllHosts(backupConn, examplePluginExec)
		// The plugin appends to its log across the whole suite, so start clean.
		Expect(os.RemoveAll(examplePluginLogPath)).To(Succeed())

		output := gpbackup(gpbackupPath, backupHelperPath,
			coordinatorOnlyBackupArgs("--plugin-config", examplePluginTestConfig)...)
		timestamp := getBackupTimestamp(string(output))
		Expect(timestamp).ToNot(BeEmpty())

		assertPluginBackedUpDataOnCoordinator(timestamp, len(coordinatorOnlyTables))

		forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb",
			"--plugin-config", examplePluginTestConfig)

		assertCoordinatorOnlyDataRestored(restoreConn, coordinatorOnlyTables)
	})

	It("backs up and restores coordinator-only tables through a plugin with --single-data-file", func() {
		// Nothing in this set writes to a segment, so the backup starts no
		// helpers and uploads no segment TOCs.  The restore must not ask the
		// plugin for them either -- it used to, and died with "Unable to process
		// segment TOC files using plugin".
		copyPluginToAllHosts(backupConn, examplePluginExec)
		Expect(os.RemoveAll(examplePluginLogPath)).To(Succeed())

		output := gpbackup(gpbackupPath, backupHelperPath,
			coordinatorOnlyBackupArgs("--plugin-config", examplePluginTestConfig, "--single-data-file")...)
		timestamp := getBackupTimestamp(string(output))
		Expect(timestamp).ToNot(BeEmpty())

		// Even under --single-data-file the coordinator talks to the plugin
		// itself; there is no helper on the coordinator to do it.
		assertPluginBackedUpDataOnCoordinator(timestamp, len(coordinatorOnlyTables))

		forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb",
			"--plugin-config", examplePluginTestConfig)

		assertCoordinatorOnlyDataRestored(restoreConn, coordinatorOnlyTables)
	})
})
