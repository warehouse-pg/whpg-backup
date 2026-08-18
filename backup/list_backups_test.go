package backup

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/spf13/pflag"
	"github.com/warehouse-pg/common-go-libs/gplog"
	"github.com/warehouse-pg/common-go-libs/operating"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// useListBackupsFlags points cmdFlags at a fresh list-backups flag set with the given
// flag=value overrides applied, mirroring what cobra does before calling DoListBackups.
func useListBackupsFlags(overrides map[string]string) *pflag.FlagSet {
	fs := pflag.NewFlagSet("list-backups", pflag.ContinueOnError)
	RegisterListBackupsFlags(fs)
	for name, value := range overrides {
		Expect(fs.Set(name, value)).To(Succeed())
	}
	UseCmdFlags(fs)
	return fs
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns everything fn wrote.
// DoListBackups writes directly to os.Stdout (tabwriter/json.Encoder), bypassing the gplog
// buffers the rest of the suite captures, so tests that check its actual output need this.
func captureStdout(fn func()) string {
	real := os.Stdout
	r, w, err := os.Pipe()
	Expect(err).ToNot(HaveOccurred())
	os.Stdout = w

	fn()

	Expect(w.Close()).To(Succeed())
	os.Stdout = real
	out, err := io.ReadAll(r)
	Expect(err).ToNot(HaveOccurred())
	return string(out)
}

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

	Describe("DoListBackups", func() {
		var tmpDir string

		BeforeEach(func() {
			tmpDir = GinkgoT().TempDir()
			operating.System.Getenv = func(key string) string {
				if key == "COORDINATOR_DATA_DIRECTORY" {
					return tmpDir
				}
				return ""
			}
		})

		Context("--format=json", func() {
			It("prints an empty JSON array instead of crashing when no history db exists", func() {
				useListBackupsFlags(map[string]string{options.FORMAT: "json"})

				var output string
				Expect(func() {
					output = captureStdout(DoListBackups)
				}).ToNot(Panic())

				Expect(strings.TrimSpace(output)).To(Equal("[]"))
				_, statErr := os.Stat(filepath.Join(tmpDir, "gpbackup_history.db"))
				Expect(os.IsNotExist(statErr)).To(BeTrue(), "list-backups must not create a history db as a side effect")
			})
		})

		Context("--quiet and --debug", func() {
			It("does not crash and raises gplog verbosity to LOGERROR for --quiet", func() {
				useListBackupsFlags(map[string]string{options.QUIET: "true"})
				Expect(func() { captureStdout(DoListBackups) }).ToNot(Panic())
				Expect(gplog.GetVerbosity()).To(Equal(gplog.LOGERROR))
			})
			It("does not crash and raises gplog verbosity to LOGDEBUG for --debug", func() {
				useListBackupsFlags(map[string]string{options.DEBUG: "true"})
				Expect(func() { captureStdout(DoListBackups) }).ToNot(Panic())
				Expect(gplog.GetVerbosity()).To(Equal(gplog.LOGDEBUG))
			})
		})

		Context("--show-all", func() {
			var activeTimestamp, deletedTimestamp string

			BeforeEach(func() {
				historyDBPath := filepath.Join(tmpDir, "gpbackup_history.db")
				db, err := history.InitializeHistoryDatabase(historyDBPath)
				Expect(err).ToNot(HaveOccurred())
				defer db.Close()

				activeTimestamp = "20260101000000"
				deletedTimestamp = "20260102000000"
				newConfig := func(ts string) history.BackupConfig {
					return history.BackupConfig{
						DatabaseName:     "testdb",
						ExcludeRelations: []string{},
						ExcludeSchemas:   []string{},
						IncludeRelations: []string{},
						IncludeSchemas:   []string{},
						RestorePlan:      []history.RestorePlanEntry{{Timestamp: ts}},
						Timestamp:        ts,
					}
				}
				active := newConfig(activeTimestamp)
				deleted := newConfig(deletedTimestamp)
				Expect(history.StoreBackupHistory(db, &active)).To(Succeed())
				Expect(history.StoreBackupHistory(db, &deleted)).To(Succeed())
				Expect(history.SetDateDeleted(db, deletedTimestamp, "20260103000000")).To(Succeed())
			})

			It("hides fully-deleted backups by default", func() {
				useListBackupsFlags(map[string]string{options.FORMAT: "json"})
				output := captureStdout(DoListBackups)
				Expect(output).To(ContainSubstring(activeTimestamp))
				Expect(output).ToNot(ContainSubstring(deletedTimestamp))
			})

			It("shows fully-deleted backups when set", func() {
				useListBackupsFlags(map[string]string{options.FORMAT: "json", options.SHOW_ALL: "true"})
				output := captureStdout(DoListBackups)
				Expect(output).To(ContainSubstring(activeTimestamp))
				Expect(output).To(ContainSubstring(deletedTimestamp))
			})
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
