package backup

import (
	"github.com/greenplum-db/gpbackup/history"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("list-backups internal tests", func() {
	Describe("isFullyDeleted", func() {
		It("treats an empty date as not deleted", func() {
			Expect(isFullyDeleted("")).To(BeFalse())
		})
		It("treats known in-progress/failed sentinels as not (fully) deleted", func() {
			Expect(isFullyDeleted(deleteStatusInProgress)).To(BeFalse())
			Expect(isFullyDeleted(deleteStatusPluginFailed)).To(BeFalse())
			Expect(isFullyDeleted(deleteStatusLocalFailed)).To(BeFalse())
		})
		It("treats any other non-empty value as a completed deletion", func() {
			Expect(isFullyDeleted("Wed Jul 21 2021 19:24:59")).To(BeTrue())
		})
	})

	Describe("backupTypeString", func() {
		It("returns full by default", func() {
			Expect(backupTypeString(history.BackupConfig{})).To(Equal("full"))
		})
		It("returns incremental when set, taking priority over data/metadata only", func() {
			Expect(backupTypeString(history.BackupConfig{Incremental: true, DataOnly: true})).To(Equal("incremental"))
		})
		It("returns data-only and metadata-only", func() {
			Expect(backupTypeString(history.BackupConfig{DataOnly: true})).To(Equal("data-only"))
			Expect(backupTypeString(history.BackupConfig{MetadataOnly: true})).To(Equal("metadata-only"))
		})
	})

	Describe("objectFilteringString", func() {
		It("returns empty when no filters were used", func() {
			Expect(objectFilteringString(history.BackupConfig{})).To(Equal(""))
		})
		It("lists every filter flag that was set", func() {
			result := objectFilteringString(history.BackupConfig{
				IncludeSchemaFiltered: true,
				ExcludeTableFiltered:  true,
			})
			Expect(result).To(Equal("include-schema, exclude-table"))
		})
	})

	Describe("backupDuration", func() {
		It("computes HH:MM:SS between two timestamps", func() {
			Expect(backupDuration("20210721190727", "20210721191449")).To(Equal("00:07:22"))
		})
		It("returns empty on unparseable input", func() {
			Expect(backupDuration("not-a-timestamp", "20210721191449")).To(Equal(""))
		})
	})

	Describe("formatHistoryTimestamp", func() {
		It("formats a valid timestamp", func() {
			Expect(formatHistoryTimestamp("20210721191330")).To(Equal("Wed Jul 21 2021 19:13:30"))
		})
		It("falls back to the raw string on unparseable input", func() {
			Expect(formatHistoryTimestamp("garbage")).To(Equal("garbage"))
		})
	})
})
