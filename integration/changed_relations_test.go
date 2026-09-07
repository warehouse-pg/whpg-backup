package integration

import (
	"fmt"

	"github.com/greenplum-db/gpbackup/backup"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/testutils"
	"github.com/spf13/cobra"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/gplog"
	"github.com/warehouse-pg/common-go-libs/testhelper"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

/*
 * A table whose storage is replaced between the backup snapshot and LockTables
 * cannot be read consistently under that snapshot. These tests run the DDL on
 * a second connection after the backup transaction has fixed its snapshot and
 * before the tables are locked, which is exactly the window gpbackup exposes.
 */
var _ = Describe("Tables changed between snapshot and lock", func() {
	const (
		aoTable     = "public.ao_changed"
		heapTable   = "public.heap_changed"
		stableTable = "public.ao_stable"
	)
	var ddlConn *dbconn.DBConn

	// beginBackupTransaction opens the backup transaction on the pool and runs
	// a first catalog read so the repeatable-read snapshot is fixed now, before
	// the concurrent DDL of each test.
	// gpbackup sets its session GUCs inside this transaction, so they are
	// part of what a retry has to preserve.
	beginBackupTransaction := func() {
		connectionPool.MustBegin(0)
		if connectionPool.Version.AtLeast(backup.SNAPSHOT_GPDB_MIN_VERSION) {
			snapshot, err := backup.GetSynchronizedSnapshot(connectionPool)
			Expect(err).ToNot(HaveOccurred())
			backup.SetBackupSnapshot(snapshot)
		}
		backup.SetSessionGUCs(0)
	}

	searchPath := func() string {
		return dbconn.MustSelectString(connectionPool, "SELECT current_setting('search_path') AS string")
	}

	lockedRelations := func() []backup.Relation {
		included := backup.GetIncludedUserTableRelations(connectionPool, backup.IncludedRelationFqns)
		relations := backup.ConvertRelationsOptionsToBackup(included)
		backup.LockTables(connectionPool, relations)
		return relations
	}

	fqns := func(changed []backup.ChangedRelation) []string {
		result := make([]string, 0, len(changed))
		for _, rel := range changed {
			result = append(result, rel.FQN())
		}
		return result
	}

	dataTableFQNs := func(tables []backup.Table) []string {
		result := make([]string, 0, len(tables))
		for _, table := range tables {
			result = append(result, table.FQN())
		}
		return result
	}

	BeforeEach(func() {
		gplog.SetVerbosity(gplog.LOGERROR) // turn off the progress bar in LockTables
		var rootCmd = &cobra.Command{}
		backup.DoInit(rootCmd) // initializes objectCounts, but also replaces the test logger
		_, stderr, logFile = testhelper.SetupTestLogger()
		backup.UseCmdFlags(backupCmdFlags)
		backup.SetMaxSnapshotAttempts(3)
		// The suite keeps search_path at pg_catalog; start each test from the
		// server default so only the GUCs set inside the transaction count.
		testhelper.AssertQueryRuns(connectionPool, "RESET search_path")

		testhelper.AssertQueryRuns(connectionPool, fmt.Sprintf(`
			CREATE TABLE %s (i int) WITH (appendonly=true) DISTRIBUTED BY (i);
			INSERT INTO %s SELECT generate_series(1, 1000);
			CREATE TABLE %s (i int) DISTRIBUTED BY (i);
			INSERT INTO %s SELECT generate_series(1, 100);
			CREATE TABLE %s (i int) WITH (appendonly=true) DISTRIBUTED BY (i);
			INSERT INTO %s SELECT generate_series(1, 10);`,
			aoTable, aoTable, heapTable, heapTable, stableTable, stableTable))

		for _, table := range []string{aoTable, heapTable, stableTable} {
			_ = backupCmdFlags.Set(options.INCLUDE_RELATION, table)
		}
		opts, err := options.NewOptions(backupCmdFlags)
		Expect(err).ToNot(HaveOccurred())
		backup.ValidateAndProcessFilterLists(opts)

		ddlConn = testutils.SetupTestDbConn("testdb")
	})
	AfterEach(func() {
		if connectionPool.Tx[0] != nil {
			_ = connectionPool.Rollback(0)
		}
		backup.SetBackupSnapshot("")
		backup.SetMaxSnapshotAttempts(3)
		testhelper.AssertQueryRuns(connectionPool, "SET search_path TO pg_catalog")
		ddlConn.Close()
		testhelper.AssertQueryRuns(connectionPool, fmt.Sprintf(
			"DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s",
			aoTable, heapTable, stableTable))
	})

	Describe("GetRelationsChangedSinceSnapshot", func() {
		It("reports nothing when no table changed", func() {
			beginBackupTransaction()
			relations := lockedRelations()

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, relations)
			Expect(changed).To(BeEmpty())
		})
		It("reports an AO table rewritten by ALTER TABLE SET WITH (reorganize=true)", func() {
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf("ALTER TABLE %s SET WITH (reorganize=true)", aoTable))
			relations := lockedRelations()

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, relations)
			Expect(fqns(changed)).To(ConsistOf(aoTable))
			Expect(changed[0].Dropped).To(BeFalse())
		})
		It("reports a truncated heap table", func() {
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf("TRUNCATE %s", heapTable))
			relations := lockedRelations()

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, relations)
			Expect(fqns(changed)).To(ConsistOf(heapTable))
			Expect(changed[0].Dropped).To(BeFalse())
		})
		It("reports a table dropped and recreated under the same name as dropped", func() {
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf(
				"DROP TABLE %s; CREATE TABLE %s (i int) WITH (appendonly=true) DISTRIBUTED BY (i)", aoTable, aoTable))
			relations := lockedRelations()

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, relations)
			Expect(fqns(changed)).To(ConsistOf(aoTable))
			Expect(changed[0].Dropped).To(BeTrue())
		})
		It("reports every changed table when several change at once", func() {
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf(
				"ALTER TABLE %s SET WITH (reorganize=true); ALTER TABLE %s SET WITH (reorganize=true)", aoTable, heapTable))
			relations := lockedRelations()

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, relations)
			Expect(fqns(changed)).To(ConsistOf(aoTable, heapTable))
		})
	})

	Describe("RetrieveAndProcessTables", func() {
		It("backs up all tables with no warning when nothing changed", func() {
			logStart := len(logFile.Contents())
			beginBackupTransaction()

			metadataTables, dataTables := backup.RetrieveAndProcessTables()

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable))
			Expect(dataTableFQNs(metadataTables)).To(ConsistOf(aoTable, heapTable, stableTable))
			Expect(backup.GetSkippedDataTables()).To(BeEmpty())
			Expect(string(logFile.Contents()[logStart:])).ToNot(ContainSubstring("changed on disk"))

			aoMetadata := backup.GetAOIncrementalMetadata(connectionPool)
			Expect(aoMetadata).To(HaveKey(aoTable))
			Expect(aoMetadata).To(HaveKey(stableTable))
		})
		It("retries with a new snapshot and backs up a table rewritten in the window", func() {
			logStart := len(logFile.Contents())
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf(
				"ALTER TABLE %s SET WITH (reorganize=true); ALTER TABLE %s SET WITH (reorganize=true)", aoTable, heapTable))

			_, dataTables := backup.RetrieveAndProcessTables()

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable))
			Expect(backup.GetSkippedDataTables()).To(BeEmpty())
			logged := string(logFile.Contents()[logStart:])
			Expect(logged).To(ContainSubstring("[WARNING]:-"))
			Expect(logged).To(ContainSubstring("changed on disk after the backup snapshot was taken"))
			Expect(logged).To(ContainSubstring(aoTable))
			Expect(logged).To(ContainSubstring(heapTable))
			Expect(logged).To(ContainSubstring("taking a new snapshot (attempt 2 of 3)"))
			Expect(logged).ToNot(ContainSubstring("not backed up"))

			// The new transaction carries the same session GUCs as the first
			// one, otherwise object names would be dumped unqualified.
			Expect(searchPath()).To(Equal("pg_catalog"))

			// The new snapshot sees the rewritten storage: the AO helper resolves
			// and the row counts are the current ones.
			aoMetadata := backup.GetAOIncrementalMetadata(connectionPool)
			Expect(aoMetadata).To(HaveKey(aoTable))
			Expect(dbconn.MustSelectString(connectionPool,
				fmt.Sprintf("SELECT count(*)::text AS string FROM %s", heapTable))).To(Equal("100"))
		})
		It("re-resolves the include list when a table was dropped and recreated in the window", func() {
			oldOid := backup.IncludedRelationFqns[0].Oid
			for _, rel := range backup.IncludedRelationFqns {
				if rel.Schema == "public" && rel.Name == "ao_changed" {
					oldOid = rel.Oid
				}
			}
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf(`DROP TABLE %s;
				CREATE TABLE %s (i int) WITH (appendonly=true) DISTRIBUTED BY (i);
				INSERT INTO %s SELECT generate_series(1, 7)`, aoTable, aoTable, aoTable))

			_, dataTables := backup.RetrieveAndProcessTables()

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable))
			for _, table := range dataTables {
				if table.FQN() == aoTable {
					Expect(table.Oid).ToNot(Equal(oldOid))
				}
			}
			Expect(searchPath()).To(Equal("pg_catalog"))
			Expect(backup.GetAOIncrementalMetadata(connectionPool)).To(HaveKey(aoTable))
			Expect(dbconn.MustSelectString(connectionPool,
				fmt.Sprintf("SELECT count(*)::text AS string FROM %s", aoTable))).To(Equal("7"))
		})
		It("skips the data of a table still changed after the last attempt and keeps its DDL", func() {
			backup.SetMaxSnapshotAttempts(1)
			logStart := len(logFile.Contents())
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf("ALTER TABLE %s SET WITH (reorganize=true)", aoTable))

			metadataTables, dataTables := backup.RetrieveAndProcessTables()

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(heapTable, stableTable))
			Expect(dataTableFQNs(metadataTables)).To(ConsistOf(aoTable, heapTable, stableTable))
			Expect(backup.GetSkippedDataTables()).To(HaveKey(aoTable))
			logged := string(logFile.Contents()[logStart:])
			Expect(logged).To(ContainSubstring("[WARNING]:-"))
			Expect(logged).To(ContainSubstring(fmt.Sprintf("Data for table %s not backed up", aoTable)))

			// The stale pg_aoseg name of the skipped table must not be queried.
			aoMetadata := backup.GetAOIncrementalMetadata(connectionPool)
			Expect(aoMetadata).ToNot(HaveKey(aoTable))
			Expect(aoMetadata).To(HaveKey(stableTable))
		})
	})
})
