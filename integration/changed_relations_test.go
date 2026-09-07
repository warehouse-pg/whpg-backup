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
		partTable   = "public.part_changed"
		partLeaf    = "public.part_changed_1_prt_2"
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

	// truncateLeafSQL empties the second partition and reloads the same rows,
	// so the leaf gets a new relfilenode while the row count stays the same.
	truncateLeafSQL := func() string {
		truncate := fmt.Sprintf("TRUNCATE %s", partLeaf)
		if connectionPool.Version.Before("7") {
			truncate = fmt.Sprintf("ALTER TABLE %s TRUNCATE PARTITION FOR (RANK(2))", partTable)
		}
		return fmt.Sprintf("%s; INSERT INTO %s SELECT g, 2 FROM generate_series(1, 10) g", truncate, partTable)
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
			INSERT INTO %s SELECT generate_series(1, 10);
			CREATE TABLE %s (id int, d int) WITH (appendonly=true) DISTRIBUTED BY (id)
				PARTITION BY RANGE (d) (START (1) END (4) EVERY (1));
			INSERT INTO %s SELECT g, (g %% 3) + 1 FROM generate_series(1, 30) g;`,
			aoTable, aoTable, heapTable, heapTable, stableTable, stableTable, partTable, partTable))

		for _, table := range []string{aoTable, heapTable, stableTable, partTable} {
			_ = backupCmdFlags.Set(options.INCLUDE_RELATION, table)
		}
		opts, err := options.NewOptions(backupCmdFlags)
		Expect(err).ToNot(HaveOccurred())
		backup.ValidateAndProcessFilterLists(opts)
		includeOids := backup.GetOidsFromRelationList(backup.IncludedRelationFqns)
		Expect(backup.ExpandIncludesForPartitions(connectionPool, opts, includeOids, backupCmdFlags)).To(Succeed())

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
			"DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s; DROP TABLE IF EXISTS %s",
			aoTable, heapTable, stableTable, partTable))
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
		It("reports a heap table rewritten by VACUUM FULL", func() {
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, fmt.Sprintf("VACUUM FULL %s", heapTable))
			relations := lockedRelations()

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, relations)
			Expect(fqns(changed)).To(ConsistOf(heapTable))
			Expect(changed[0].Dropped).To(BeFalse())
		})
		It("reports a truncated partition below a locked table with the table as its ancestor", func() {
			// Without --leaf-partition-data the backup locks and copies the
			// parent; the check has to reach the partition through pg_inherits.
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, truncateLeafSQL())
			relations := lockedRelations()

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, relations)
			Expect(fqns(changed)).To(ConsistOf(partLeaf))
			Expect(changed[0].Dropped).To(BeFalse())
			Expect(changed[0].Ancestors).ToNot(BeEmpty())
			Expect(changed[0].Ancestors[len(changed[0].Ancestors)-1].FQN()).To(Equal(partTable))
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

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable, partTable))
			Expect(dataTableFQNs(metadataTables)).To(ContainElements(aoTable, heapTable, stableTable, partTable))
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

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable, partTable))
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

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable, partTable))
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

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(heapTable, stableTable, partTable))
			Expect(dataTableFQNs(metadataTables)).To(ContainElements(aoTable, heapTable, stableTable, partTable))
			Expect(backup.GetSkippedDataTables()).To(HaveKey(aoTable))
			logged := string(logFile.Contents()[logStart:])
			Expect(logged).To(ContainSubstring("[WARNING]:-"))
			Expect(logged).To(ContainSubstring(fmt.Sprintf("Data for table %s not backed up", aoTable)))

			// The stale pg_aoseg name of the skipped table must not be queried.
			aoMetadata := backup.GetAOIncrementalMetadata(connectionPool)
			Expect(aoMetadata).ToNot(HaveKey(aoTable))
			Expect(aoMetadata).To(HaveKey(stableTable))
		})
		It("retries when a partition of a table copied through its parent is truncated in the window", func() {
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, truncateLeafSQL())

			_, dataTables := backup.RetrieveAndProcessTables()

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable, partTable))
			Expect(backup.GetSkippedDataTables()).To(BeEmpty())
			// Without --leaf-partition-data the incremental entry is the parent's
			// on 6.x, where the parent has storage, and the leaf's on 7.x, where
			// it has none.
			aoMetadata := backup.GetAOIncrementalMetadata(connectionPool)
			if connectionPool.Version.Before("7") {
				Expect(aoMetadata).To(HaveKey(partTable))
			} else {
				Expect(aoMetadata).To(HaveKey(partLeaf))
			}
			Expect(dbconn.MustSelectString(connectionPool,
				fmt.Sprintf("SELECT count(*)::text AS string FROM %s", partTable))).To(Equal("30"))
		})
		It("skips the data of the parent when a partition keeps changing, and keeps the parent's DDL", func() {
			// The parent is the table whose COPY reads the partition, so it is
			// the one that has to leave the data set.
			backup.SetMaxSnapshotAttempts(1)
			logStart := len(logFile.Contents())
			beginBackupTransaction()
			testhelper.AssertQueryRuns(ddlConn, truncateLeafSQL())

			metadataTables, dataTables := backup.RetrieveAndProcessTables()

			Expect(dataTableFQNs(dataTables)).To(ConsistOf(aoTable, heapTable, stableTable))
			Expect(dataTableFQNs(metadataTables)).To(ContainElement(partTable))
			Expect(backup.GetSkippedDataTables()).To(HaveKey(partTable))
			logged := string(logFile.Contents()[logStart:])
			Expect(logged).To(ContainSubstring(fmt.Sprintf("partition of %s", partTable)))
			Expect(logged).To(ContainSubstring(fmt.Sprintf("Data for table %s not backed up", partTable)))

			aoMetadata := backup.GetAOIncrementalMetadata(connectionPool)
			Expect(aoMetadata).ToNot(HaveKey(partLeaf))
			Expect(aoMetadata).ToNot(HaveKey(partTable))
			Expect(aoMetadata).To(HaveKey(stableTable))
		})
	})
})
