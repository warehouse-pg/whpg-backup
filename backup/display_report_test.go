package backup

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/spf13/pflag"
	"github.com/warehouse-pg/common-go-libs/gplog"
	"github.com/warehouse-pg/common-go-libs/operating"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// useDisplayReportFlags points cmdFlags at a fresh display-report flag set with the given
// flag=value overrides applied, mirroring what cobra does before calling DoDisplayReport.
func useDisplayReportFlags(overrides map[string]string) *pflag.FlagSet {
	fs := pflag.NewFlagSet("display-report", pflag.ContinueOnError)
	RegisterDisplayReportFlags(fs)
	for name, value := range overrides {
		Expect(fs.Set(name, value)).To(Succeed())
	}
	UseCmdFlags(fs)
	return fs
}

var _ = Describe("display-report internal tests", func() {
	Describe("parseReportText", func() {
		It("splits header fields from the object-count section", func() {
			text := "Greenplum Database Backup Report\n\n" +
				"timestamp key:         20260101000000\n" +
				"whpg version:          1.0.0\n\n" +
				"backup status:         Success\n\n" +
				"count of database objects in backup:\n" +
				"tables                       2\n" +
				"views                        0\n"

			fields, objectCounts := parseReportText(text)
			Expect(fields).To(HaveKeyWithValue("timestamp_key", "20260101000000"))
			Expect(fields).To(HaveKeyWithValue("whpg_version", "1.0.0"))
			Expect(fields).To(HaveKeyWithValue("backup_status", "Success"))
			Expect(objectCounts).To(HaveKeyWithValue("tables", 2))
			Expect(objectCounts).To(HaveKeyWithValue("views", 0))
		})

		It("keeps only the first colon so a value containing one is preserved", func() {
			text := "command line:          gpbackup --dbname foo\n\ncount of database objects in backup:\ntables 1\n"
			fields, _ := parseReportText(text)
			Expect(fields["command_line"]).To(Equal("gpbackup --dbname foo"))
		})

		It("folds a colonless continuation line into the most recently seen key", func() {
			text := "incremental backup set:\n20260101000000\n20260102000000\n\ncount of database objects in backup:\ntables 1\n"
			fields, objectCounts := parseReportText(text)
			Expect(fields).ToNot(HaveKey("20260101000000"))
			Expect(fields["incremental_backup_set"]).To(Equal("\n20260101000000\n20260102000000"))
			Expect(objectCounts).To(HaveKeyWithValue("tables", 1))
		})

		It("resets the continuation key at a blank line so unrelated lines aren't merged", func() {
			text := "backup status:         Success\n\ndatabase size:         81 MB\n\ncount of database objects in backup:\ntables 1\n"
			fields, _ := parseReportText(text)
			Expect(fields["backup_status"]).To(Equal("Success"))
			Expect(fields["database_size"]).To(Equal("81 MB"))
		})
	})

	Describe("DoDisplayReport", func() {
		var (
			tmpDir    string
			timestamp string
			reportDir string
			reportTxt string
		)

		BeforeEach(func() {
			tmpDir = GinkgoT().TempDir()
			operating.System.Getenv = func(key string) string {
				if key == "COORDINATOR_DATA_DIRECTORY" {
					return tmpDir
				}
				return ""
			}

			timestamp = "20260819113735"
			reportDir = filepath.Join(tmpDir, "backups", timestamp[0:8], timestamp)
			Expect(os.MkdirAll(reportDir, 0755)).To(Succeed())

			reportTxt = "Greenplum Database Backup Report\n\n" +
				"timestamp key:         " + timestamp + "\n" +
				"whpg version:          1.0.0\n\n" +
				"backup status:         Success\n\n" +
				"count of database objects in backup:\n" +
				"tables                       2\n"
		})

		AfterEach(func() {
			operating.System = operating.InitializeSystemFunctions()
		})

		storeBackup := func(config history.BackupConfig) {
			historyDBPath := filepath.Join(tmpDir, "gpbackup_history.db")
			db, err := history.InitializeHistoryDatabase(historyDBPath)
			Expect(err).ToNot(HaveOccurred())
			defer db.Close()
			Expect(history.StoreBackupHistory(db, &config)).To(Succeed())
		}

		writeReportFile := func(contents string) {
			reportPath := filepath.Join(reportDir, "gpbackup_"+timestamp+"_report")
			Expect(os.WriteFile(reportPath, []byte(contents), 0444)).To(Succeed())
		}

		newConfig := func() history.BackupConfig {
			return history.BackupConfig{
				DatabaseName:     "testdb",
				Status:           history.BackupStatusSucceed,
				ExcludeRelations: []string{},
				ExcludeSchemas:   []string{},
				IncludeRelations: []string{},
				IncludeSchemas:   []string{},
				RestorePlan:      []history.RestorePlanEntry{},
				Timestamp:        timestamp,
			}
		}

		It("panics when the timestamp is not a valid 14-digit format", func() {
			useDisplayReportFlags(nil)
			Expect(func() { DoDisplayReport("not-a-timestamp") }).To(Panic())
		})

		It("panics on an invalid --format value, before ever touching the history db", func() {
			useDisplayReportFlags(map[string]string{options.FORMAT: "yaml"})
			Expect(func() { DoDisplayReport(timestamp) }).To(Panic())
		})

		It("panics when the timestamp is not found in the history database", func() {
			useDisplayReportFlags(nil)
			Expect(func() { DoDisplayReport(timestamp) }).To(Panic())
		})

		It("panics when a plugin-backed backup's report is missing locally and no --plugin-config was given", func() {
			config := newConfig()
			config.Plugin = "some_plugin"
			storeBackup(config)

			useDisplayReportFlags(nil)
			Expect(func() { DoDisplayReport(timestamp) }).To(Panic())
		})

		Context("with a report file on disk", func() {
			BeforeEach(func() {
				writeReportFile(reportTxt)
				storeBackup(newConfig())
			})

			It("prints the raw report file for --format=text (default)", func() {
				useDisplayReportFlags(nil)
				output := captureStdout(func() { DoDisplayReport(timestamp) })
				Expect(output).To(Equal(reportTxt))
			})

			It("prints structured JSON, with object_counts nested and fields flattened, for --format=json", func() {
				useDisplayReportFlags(map[string]string{options.FORMAT: "json"})
				output := captureStdout(func() { DoDisplayReport(timestamp) })

				var entry map[string]interface{}
				Expect(json.Unmarshal([]byte(output), &entry)).To(Succeed())
				Expect(entry["timestamp"]).To(Equal(timestamp))
				Expect(entry["database"]).To(Equal("testdb"))
				Expect(entry["status"]).To(Equal(history.BackupStatusSucceed))
				Expect(entry["whpg_version"]).To(Equal("1.0.0"))
				Expect(entry).ToNot(HaveKey("fields"))

				objectCounts, ok := entry["object_counts"].(map[string]interface{})
				Expect(ok).To(BeTrue())
				Expect(objectCounts["tables"]).To(Equal(float64(2)))
			})

			It("does not crash and raises gplog verbosity to LOGERROR for --quiet", func() {
				useDisplayReportFlags(map[string]string{options.QUIET: "true"})
				Expect(func() { captureStdout(func() { DoDisplayReport(timestamp) }) }).ToNot(Panic())
				Expect(gplog.GetVerbosity()).To(Equal(gplog.LOGERROR))
			})

			It("does not crash and raises gplog verbosity to LOGDEBUG for --debug", func() {
				useDisplayReportFlags(map[string]string{options.DEBUG: "true"})
				Expect(func() { captureStdout(func() { DoDisplayReport(timestamp) }) }).ToNot(Panic())
				Expect(gplog.GetVerbosity()).To(Equal(gplog.LOGDEBUG))
			})

			It("does not crash and raises gplog verbosity to LOGVERBOSE for --verbose", func() {
				useDisplayReportFlags(map[string]string{options.VERBOSE: "true"})
				Expect(func() { captureStdout(func() { DoDisplayReport(timestamp) }) }).ToNot(Panic())
				Expect(gplog.GetVerbosity()).To(Equal(gplog.LOGVERBOSE))
			})
		})
	})
})
