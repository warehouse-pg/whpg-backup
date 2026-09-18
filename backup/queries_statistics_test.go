package backup_test

import (
	"github.com/greenplum-db/gpbackup/backup"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("backup/queries_statistics", func() {
	Describe("statistics query builders with an empty table set", func() {
		// A backup that skips every table's data (all of them kept changing)
		// passes no tables here. The builders must return nothing without
		// running a query, since an empty IN () list is a syntax error.
		It("GetAttributeStatistics returns no statistics and runs no query", func() {
			result := backup.GetAttributeStatistics(connectionPool, []backup.Table{})
			Expect(result).To(BeEmpty())
			Expect(mock.ExpectationsWereMet()).To(Succeed())
		})
		It("GetTupleStatistics returns no statistics and runs no query", func() {
			result := backup.GetTupleStatistics(connectionPool, []backup.Table{})
			Expect(result).To(BeEmpty())
			Expect(mock.ExpectationsWereMet()).To(Succeed())
		})
	})
})
