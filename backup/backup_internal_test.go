package backup

import (
	"errors"
	"sync"

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
})
