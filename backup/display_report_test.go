package backup

import (
	"encoding/json"
	"fmt"
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

		It("skips the whole backup error section instead of splitting its own colons into bogus keys", func() {
			text := "backup error:          could not dispatch to segment seg0\n" +
				"connection refused: server closed\n\n" +
				"database size:         81 MB\n\n" +
				"count of database objects in backup:\ntables 1\n"
			fields, _ := parseReportText(text)
			Expect(fields).ToNot(HaveKey("backup_error"))
			Expect(fields).ToNot(HaveKey("connection_refused"))
			Expect(fields["database_size"]).To(Equal("81 MB"))
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

		writeConfigFile := func(config history.BackupConfig) {
			configPath := filepath.Join(reportDir, "gpbackup_"+timestamp+"_config.yaml")
			_ = os.Remove(configPath) // WriteConfigFile makes the file read-only; clear any prior write first
			history.WriteConfigFile(&config, configPath)
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

		Context("with a report file only fetchable from a plugin", func() {
			// writePluginConfig writes a fake plugin executable that, given `restore_file <config>
			// <path>`, writes reportTxt or a minimal BackupConfig YAML to <path> depending on the
			// destination filename, mirroring the fake-plugin-script pattern used in
			// backup/delete_backup_test.go.
			writePluginConfig := func(dir string) string {
				scriptPath := filepath.Join(dir, "fake_plugin.sh")
				script := fmt.Sprintf(`#!/bin/bash
case "$3" in
  *_config.yaml)
    cat > "$3" << 'EOF'
databasename: testdb
timestamp: %s
EOF
    ;;
  *)
    cat > "$3" << 'EOF'
%s
EOF
    ;;
esac
`, timestamp, reportTxt)
				Expect(os.WriteFile(scriptPath, []byte(script), 0755)).To(Succeed())

				configPath := filepath.Join(dir, "plugin_config.yaml")
				Expect(os.WriteFile(configPath, []byte(fmt.Sprintf("executablepath: %q\n", scriptPath)), 0644)).To(Succeed())
				return configPath
			}

			It("fetches the report via the plugin, prints it, and removes the fetched local copy afterward", func() {
				config := newConfig()
				config.Plugin = "some_plugin"
				storeBackup(config)

				pluginConfigPath := writePluginConfig(tmpDir)
				reportPath := filepath.Join(reportDir, "gpbackup_"+timestamp+"_report")

				useDisplayReportFlags(map[string]string{options.PLUGIN_CONFIG: pluginConfigPath})
				Expect(func() { captureStdout(func() { DoDisplayReport(timestamp) }) }).ToNot(Panic())

				_, statErr := os.Stat(reportPath)
				Expect(os.IsNotExist(statErr)).To(BeTrue())
			})

			It("does not remove a report file that already existed locally before the call", func() {
				config := newConfig()
				config.Plugin = "some_plugin"
				storeBackup(config)
				writeReportFile(reportTxt)

				pluginConfigPath := writePluginConfig(tmpDir)
				reportPath := filepath.Join(reportDir, "gpbackup_"+timestamp+"_report")

				useDisplayReportFlags(map[string]string{options.PLUGIN_CONFIG: pluginConfigPath})
				Expect(func() { captureStdout(func() { DoDisplayReport(timestamp) }) }).ToNot(Panic())

				_, statErr := os.Stat(reportPath)
				Expect(statErr).ToNot(HaveOccurred())
			})

			It("also fetches the config file via the plugin for --format=json, and removes it afterward too", func() {
				config := newConfig()
				config.Plugin = "some_plugin"
				storeBackup(config)

				pluginConfigPath := writePluginConfig(tmpDir)
				reportPath := filepath.Join(reportDir, "gpbackup_"+timestamp+"_report")
				configPath := filepath.Join(reportDir, "gpbackup_"+timestamp+"_config.yaml")

				useDisplayReportFlags(map[string]string{
					options.PLUGIN_CONFIG: pluginConfigPath,
					options.FORMAT:        "json",
				})
				output := captureStdout(func() { DoDisplayReport(timestamp) })

				var entry map[string]interface{}
				Expect(json.Unmarshal([]byte(output), &entry)).To(Succeed())
				Expect(entry["database"]).To(Equal("testdb"))

				_, statErr := os.Stat(reportPath)
				Expect(os.IsNotExist(statErr)).To(BeTrue())
				_, statErr = os.Stat(configPath)
				Expect(os.IsNotExist(statErr)).To(BeTrue())
			})
		})

		Context("with a report file on disk", func() {
			BeforeEach(func() {
				writeReportFile(reportTxt)
				storeBackup(newConfig())
				writeConfigFile(newConfig())
			})

			It("prints the raw report file for --format=text (default)", func() {
				useDisplayReportFlags(nil)
				output := captureStdout(func() { DoDisplayReport(timestamp) })
				Expect(output).To(Equal(reportTxt))
			})

			It("prints structured JSON, with history-db fields at top level and report-text fields nested under report_fields, for --format=json", func() {
				useDisplayReportFlags(map[string]string{options.FORMAT: "json"})
				output := captureStdout(func() { DoDisplayReport(timestamp) })

				var entry map[string]interface{}
				Expect(json.Unmarshal([]byte(output), &entry)).To(Succeed())
				Expect(entry["timestamp"]).To(Equal(timestamp))
				Expect(entry["database"]).To(Equal("testdb"))
				Expect(entry["status"]).To(Equal(history.BackupStatusSucceed))

				reportFields, ok := entry["report_fields"].(map[string]interface{})
				Expect(ok).To(BeTrue())
				Expect(reportFields["whpg_version"]).To(Equal("1.0.0"))

				objectCounts, ok := entry["object_counts"].(map[string]interface{})
				Expect(ok).To(BeTrue())
				Expect(objectCounts["tables"]).To(Equal(float64(2)))
			})

			It("sources backup_error from the config file's ErrorMessage, unsplit, even when it's multi-line and contains colons", func() {
				config := newConfig()
				config.ErrorMessage = "could not dispatch to segment seg0\nconnection refused: server closed"
				writeConfigFile(config)

				useDisplayReportFlags(map[string]string{options.FORMAT: "json"})
				output := captureStdout(func() { DoDisplayReport(timestamp) })

				var entry map[string]interface{}
				Expect(json.Unmarshal([]byte(output), &entry)).To(Succeed())
				Expect(entry["backup_error"]).To(Equal("could not dispatch to segment seg0\nconnection refused: server closed"))

				reportFields, ok := entry["report_fields"].(map[string]interface{})
				Expect(ok).To(BeTrue())
				Expect(reportFields).ToNot(HaveKey("backup_error"))
				Expect(reportFields).ToNot(HaveKey("connection_refused"))
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
