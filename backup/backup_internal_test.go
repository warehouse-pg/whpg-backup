package backup

import (
	"errors"
	"sync"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/pflag"
	"github.com/warehouse-pg/common-go-libs/testhelper"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
)

var _ = Describe("backup internal tests", func() {
	var log *Buffer
	BeforeEach(func() {
		_, _, log = testhelper.SetupTestLogger()
	})

	Describe("backupData", func() {
		It("returns successfully immediately if there is no table data to backup", func() {
			emptyTableSlice := make([]Table, 0)

			backupData(emptyTableSlice)
			Expect(string(log.Contents())).To(ContainSubstring("Data backup complete"))
		})
	})

	Describe("DoCleanup", func() {
		BeforeEach(func() {
			cmdFlags = pflag.NewFlagSet("gpbackup", pflag.ExitOnError)
			SetCmdFlags(cmdFlags)

			CleanupGroup = &sync.WaitGroup{}
			CleanupGroup.Add(1)

			wasTerminated = false
		})

		It("does not panic when globalCluster is nil and single-data-file is set", func() {
			globalCluster = nil
			globalFPInfo = filepath.FilePathInfo{Timestamp: "20170101010101"}
			connectionPool = nil
			backupReport = nil
			_ = cmdFlags.Set(options.SINGLE_DATA_FILE, "true")
			_ = cmdFlags.Set(options.NO_HISTORY, "true")

			DoCleanup(true)
		})

		It("does not panic when backupReport is nil and no-history is not set", func() {
			globalFPInfo = filepath.FilePathInfo{Timestamp: "20170101010101"}
			connectionPool = nil
			backupReport = nil
			_ = cmdFlags.Set(options.NO_HISTORY, "false")

			DoCleanup(true)
		})

		It("does not panic when all globals are nil", func() {
			globalCluster = nil
			globalFPInfo = filepath.FilePathInfo{}
			connectionPool = nil
			backupReport = nil
			_ = cmdFlags.Set(options.NO_HISTORY, "true")

			DoCleanup(true)
		})
	})
	Describe("isMissingRelationError", func() {
		It("is true for a table or schema that no longer exists", func() {
			Expect(isMissingRelationError(&pgconn.PgError{Code: PG_UNDEFINED_TABLE})).To(BeTrue())
			Expect(isMissingRelationError(&pgconn.PgError{Code: PG_INVALID_SCHEMA})).To(BeTrue())
		})
		It("is false for any other error", func() {
			Expect(isMissingRelationError(&pgconn.PgError{Code: PG_LOCK_NOT_AVAILABLE})).To(BeFalse())
			Expect(isMissingRelationError(errors.New("connection reset"))).To(BeFalse())
		})
	})
	Describe("tablesWithBackedUpData", func() {
		root := Table{Relation: Relation{Schema: "public", Name: "parted"}}
		leaf1 := Table{Relation: Relation{Schema: "public", Name: "parted_1_prt_1"},
			TableDefinition: TableDefinition{AttachPartitionInfo: AttachPartitionInfo{Relname: "public.parted_1_prt_1", Parent: "public.parted"}}}
		leaf2 := Table{Relation: Relation{Schema: "public", Name: "parted_1_prt_2"},
			TableDefinition: TableDefinition{AttachPartitionInfo: AttachPartitionInfo{Relname: "public.parted_1_prt_2", Parent: "public.parted"}}}
		other := Table{Relation: Relation{Schema: "public", Name: "other"}}
		AfterEach(func() {
			skippedDataTables = nil
		})
		It("keeps every table when no data was left out", func() {
			skippedDataTables = map[string]bool{}
			Expect(tablesWithBackedUpData([]Table{root, leaf1, leaf2, other})).To(Equal([]Table{root, leaf1, leaf2, other}))
		})
		It("leaves out a partition backed up on its own, but not its root or the other partitions", func() {
			// --leaf-partition-data: the changed partition is the table left out.
			skippedDataTables = map[string]bool{"public.parted_1_prt_2": true}
			Expect(tablesWithBackedUpData([]Table{root, leaf1, leaf2, other})).To(Equal([]Table{root, leaf1, other}))
		})
		It("leaves out a root copied with its partitions, and the partitions", func() {
			// Without --leaf-partition-data the root carries every partition's
			// rows, so none of them is restored.
			skippedDataTables = map[string]bool{"public.parted": true}
			Expect(tablesWithBackedUpData([]Table{root, leaf1, leaf2, other})).To(Equal([]Table{other}))
		})
	})
	Describe("restartBackupTransactions", func() {
		It("stops the backup when a connection cannot roll back", func() {
			savedPool := connectionPool
			defer func() { connectionPool = savedPool }()
			var mock sqlmock.Sqlmock
			connectionPool, mock = testhelper.CreateAndConnectMockDB(1)
			mock.ExpectBegin()
			connectionPool.MustBegin(0)
			mock.ExpectRollback().WillReturnError(errors.New("connection reset by peer"))

			defer testhelper.ShouldPanicWithMessage("connection reset by peer")
			restartBackupTransactions()
		})
	})
})
