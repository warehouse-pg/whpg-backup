package restore_test

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/greenplum-db/gpbackup/backup"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/restore"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/warehouse-pg/common-go-libs/cluster"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("restore/data tests", func() {
	Describe("NeedsRedistribution", func() {
		It("redistributes on a resize restore", func() {
			Expect(restore.NeedsRedistribution(toc.CoordinatorDataEntry{}, true)).To(BeTrue())
		})
		It("redistributes a table distributed by an enum column", func() {
			Expect(restore.NeedsRedistribution(toc.CoordinatorDataEntry{DistByEnum: true}, false)).To(BeTrue())
		})
		It("does not redistribute an ordinary table on a same-size restore", func() {
			Expect(restore.NeedsRedistribution(toc.CoordinatorDataEntry{}, false)).To(BeFalse())
		})
		It("never redistributes a coordinator-only table, even on a resize restore", func() {
			// The server rejects SET DISTRIBUTED/REORGANIZE for these tables,
			// so attempting it would fail the restore.
			Expect(restore.NeedsRedistribution(toc.CoordinatorDataEntry{IsCoordinatorOnly: true}, true)).To(BeFalse())
			Expect(restore.NeedsRedistribution(toc.CoordinatorDataEntry{IsCoordinatorOnly: true, DistByEnum: true}, false)).To(BeFalse())
		})
	})
	Describe("CopyTableIn", func() {
		BeforeEach(func() {
			utils.SetPipeThroughProgram(utils.PipeThroughProgram{Name: "cat", OutputCommand: utils.DefaultPipeThroughProgram, InputCommand: utils.DefaultPipeThroughProgram, Extension: ""})
			backup.SetPluginConfig(nil)
			_ = cmdFlags.Set(options.PLUGIN_CONFIG, "")
			backup.SetCluster(&cluster.Cluster{ContentIDs: []int{-1, 0, 1, 2}})
			restore.SetBackupConfig(&history.BackupConfig{})
		})
		It("will restore a table from its own file with gzip compression", func() {
			utils.SetPipeThroughProgram(utils.PipeThroughProgram{Name: "gzip", OutputCommand: "gzip -c -1", InputCommand: "gzip -d -c", Extension: ".gz"})
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM 'cat <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456.gz | gzip -d -c' WITH CSV DELIMITER ',' ON SEGMENT")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))
			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456.gz"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, false)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a table from its own file with zstd compression", func() {
			utils.SetPipeThroughProgram(utils.PipeThroughProgram{Name: "zstd", OutputCommand: "zstd --compress -1 -c", InputCommand: "zstd --decompress -c", Extension: ".zst"})
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM 'cat <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456.zst | zstd --decompress -c' WITH CSV DELIMITER ',' ON SEGMENT")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))
			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456.zst"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, false)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a table from its own file without compression", func() {
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM 'cat <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456' WITH CSV DELIMITER ',' ON SEGMENT")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))
			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, false)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a table from a single data file", func() {
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM 'cat <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456' WITH CSV DELIMITER ',' ON SEGMENT")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))
			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, true, 0, false)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a coordinator-only table on the coordinator, without ON SEGMENT", func() {
			utils.SetPipeThroughProgram(utils.PipeThroughProgram{Name: "gzip", OutputCommand: "gzip -c -1", InputCommand: "gzip -d -c", Extension: ".gz"})
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM 'cat /data/coordinator/backups/20170101/20170101010101/gpbackup_-1_20170101010101_3456.gz | gzip -d -c' WITH CSV DELIMITER ',';")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))
			filename := "/data/coordinator/backups/20170101/20170101010101/gpbackup_-1_20170101010101_3456.gz"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, true)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a coordinator-only table from its own file even with a single data file backup", func() {
			// The coordinator's file is not part of the segments' single data
			// file, so it must still be decompressed by the COPY itself rather
			// than by the restore helper.
			utils.SetPipeThroughProgram(utils.PipeThroughProgram{Name: "gzip", OutputCommand: "gzip -c -1", InputCommand: "gzip -d -c", Extension: ".gz"})
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM 'cat /data/coordinator/backups/20170101/20170101010101/gpbackup_-1_20170101010101_3456.gz | gzip -d -c' WITH CSV DELIMITER ',';")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))
			filename := "/data/coordinator/backups/20170101/20170101010101/gpbackup_-1_20170101010101_3456.gz"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, true, 0, true)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a coordinator-only table through a plugin", func() {
			_ = cmdFlags.Set(options.PLUGIN_CONFIG, "/tmp/plugin_config")
			pluginConfig := utils.PluginConfig{ExecutablePath: "/tmp/fake-plugin.sh", ConfigPath: "/tmp/plugin_config"}
			restore.SetPluginConfig(&pluginConfig)
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM '/tmp/fake-plugin.sh restore_data /tmp/plugin_config /data/coordinator/backups/20170101/20170101010101/gpbackup_-1_20170101010101_3456' WITH CSV DELIMITER ',';")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))
			filename := "/data/coordinator/backups/20170101/20170101010101/gpbackup_-1_20170101010101_3456"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, true)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a table from its own file with gzip compression using a plugin", func() {
			utils.SetPipeThroughProgram(utils.PipeThroughProgram{Name: "gzip", OutputCommand: "gzip -c -1", InputCommand: "gzip -d -c", Extension: ".gz"})
			_ = cmdFlags.Set(options.PLUGIN_CONFIG, "/tmp/plugin_config")
			pluginConfig := utils.PluginConfig{ExecutablePath: "/tmp/fake-plugin.sh", ConfigPath: "/tmp/plugin_config"}
			restore.SetPluginConfig(&pluginConfig)
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM '/tmp/fake-plugin.sh restore_data /tmp/plugin_config <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456.gz | gzip -d -c' WITH CSV DELIMITER ',' ON SEGMENT")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))

			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456.gz"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, false)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a table from its own file with zstd compression using a plugin", func() {
			utils.SetPipeThroughProgram(utils.PipeThroughProgram{Name: "zstd", OutputCommand: "zstd --compress -1 -c", InputCommand: "zstd --decompress -c", Extension: ".zst"})
			_ = cmdFlags.Set(options.PLUGIN_CONFIG, "/tmp/plugin_config")
			pluginConfig := utils.PluginConfig{ExecutablePath: "/tmp/fake-plugin.sh", ConfigPath: "/tmp/plugin_config"}
			restore.SetPluginConfig(&pluginConfig)
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM '/tmp/fake-plugin.sh restore_data /tmp/plugin_config <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456.zst | zstd --decompress -c' WITH CSV DELIMITER ',' ON SEGMENT")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))

			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456.zst"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, false)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will restore a table from its own file without compression using a plugin", func() {
			_ = cmdFlags.Set(options.PLUGIN_CONFIG, "/tmp/plugin_config")
			pluginConfig := utils.PluginConfig{ExecutablePath: "/tmp/fake-plugin.sh", ConfigPath: "/tmp/plugin_config"}
			restore.SetPluginConfig(&pluginConfig)
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM '/tmp/fake-plugin.sh restore_data /tmp/plugin_config <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456.gz' WITH CSV DELIMITER ',' ON SEGMENT")
			mock.ExpectExec(execStr).WillReturnResult(sqlmock.NewResult(10, 0))

			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_pipe_3456.gz"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, false)

			Expect(err).ShouldNot(HaveOccurred())
		})
		It("will output expected error string from COPY ON SEGMENT failure", func() {
			execStr := regexp.QuoteMeta("COPY public.foo(i,j) FROM PROGRAM 'cat <SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456' WITH CSV DELIMITER ',' ON SEGMENT")
			pgErr := &pgconn.PgError{
				Severity: "ERROR",
				Code:     "22P04",
				Message:  "value of distribution key doesn't belong to segment with ID 0, it belongs to segment with ID 1",
				Where:    "COPY foo, line 1: \"5\"",
			}
			mock.ExpectExec(execStr).WillReturnError(pgErr)
			filename := "<SEG_DATA_DIR>/backups/20170101/20170101010101/gpbackup_<SEGID>_20170101010101_3456"
			_, err := restore.CopyTableIn(connectionPool, "public.foo", "(i,j)", filename, false, 0, false)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("Error loading data into table public.foo: " +
				"COPY foo, line 1: \"5\": " +
				"ERROR: value of distribution key doesn't belong to segment with ID 0, it belongs to segment with ID 1 (SQLSTATE 22P04)"))
		})
	})
	Describe("CheckRowsRestored", func() {
		var (
			expectedRows int64 = 10
			name               = "public.foo"
		)
		It("does nothing if the number of rows match ", func() {
			err := restore.CheckRowsRestored(10, expectedRows, name)
			Expect(err).ToNot(HaveOccurred())
		})
		It("returns an error if the numbers of rows do not match", func() {
			err := restore.CheckRowsRestored(5, expectedRows, name)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("Expected to restore 10 rows to table public.foo, but restored 5 instead"))
		})
	})
})

func batchMapToString(m map[int]map[int]int) string {
	outer := make([]string, len(m))
	for num, batch := range m {
		outer[num] = fmt.Sprintf("%d: %s", num, contentMapToString(batch))
	}
	return strings.Join(outer, "; ")
}

func contentMapToString(m map[int]int) string {
	inner := make([]string, len(m))
	index := 0
	for orig, dest := range m {
		inner[index] = fmt.Sprintf("%d:%d", orig, dest)
		index++
	}
	sort.Strings(inner)
	return fmt.Sprintf("{%s}", strings.Join(inner, ", "))
}
