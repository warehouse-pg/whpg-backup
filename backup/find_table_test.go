package backup

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/spf13/pflag"
	"github.com/warehouse-pg/common-go-libs/operating"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v2"
)

// useFindTableFlags points cmdFlags at a fresh find-table flag set with the given flag=value
// overrides applied, mirroring what cobra does before calling DoFindTable.
func useFindTableFlags(overrides map[string]string) *pflag.FlagSet {
	fs := pflag.NewFlagSet("find-table", pflag.ContinueOnError)
	RegisterFindTableFlags(fs)
	for name, value := range overrides {
		Expect(fs.Set(name, value)).To(Succeed())
	}
	UseCmdFlags(fs)
	return fs
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns everything fn wrote.
func captureStderr(fn func()) string {
	real := os.Stderr
	r, w, err := os.Pipe()
	Expect(err).ToNot(HaveOccurred())
	os.Stderr = w

	fn()

	Expect(w.Close()).To(Succeed())
	os.Stderr = real
	out, err := io.ReadAll(r)
	Expect(err).ToNot(HaveOccurred())
	return string(out)
}

var _ = Describe("find-table internal tests", func() {
	Describe("parseTableFQN", func() {
		It("lowercases a bare, unquoted schema.table", func() {
			schema, table, err := parseTableFQN("MySchema.MyTable")
			Expect(err).To(BeNil())
			Expect(schema).To(Equal("myschema"))
			Expect(table).To(Equal("mytable"))
		})

		It("preserves case and unescapes doubled quotes for a fully-quoted identifier", func() {
			schema, table, err := parseTableFQN(`"MySchema"."My""Table"`)
			Expect(err).To(BeNil())
			Expect(schema).To(Equal("MySchema"))
			Expect(table).To(Equal(`My"Table`))
		})

		It("allows a literal dot inside a quoted identifier", func() {
			schema, table, err := parseTableFQN(`"my.schema"."my.table"`)
			Expect(err).To(BeNil())
			Expect(schema).To(Equal("my.schema"))
			Expect(table).To(Equal("my.table"))
		})

		It("folds only the unquoted side when just one side is quoted", func() {
			schema, table, err := parseTableFQN(`MySchema."MyTable"`)
			Expect(err).To(BeNil())
			Expect(schema).To(Equal("myschema"))
			Expect(table).To(Equal("MyTable"))
		})

		It("errors on a name with no dot", func() {
			_, _, err := parseTableFQN("notfullyqualified")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("tocContainsTable", func() {
		tocFile := &toc.TOC{
			DataEntries: []toc.CoordinatorDataEntry{
				{Schema: "public", Name: "foo"},
				{Schema: "public", Name: "leaf1", PartitionRoot: "partitioned"},
				{Schema: `"MySchema"`, Name: `"My""Table"`},
			},
		}

		It("matches a table with its own data entry", func() {
			Expect(tocContainsTable(tocFile, "public", "foo")).To(BeTrue())
		})

		It("matches a partition root by way of one of its leaves", func() {
			Expect(tocContainsTable(tocFile, "public", "partitioned")).To(BeTrue())
		})

		It("does not match a table with no data entry", func() {
			Expect(tocContainsTable(tocFile, "public", "bar")).To(BeFalse())
		})

		It("does not match across schemas", func() {
			Expect(tocContainsTable(tocFile, "other", "foo")).To(BeFalse())
		})

		It("matches a quote_ident-quoted entry against its unquoted identifier value", func() {
			Expect(tocContainsTable(tocFile, "MySchema", `My"Table`)).To(BeTrue())
		})
	})

	Describe("findBackupsContainingTable", func() {
		var historyDBPath, coordinatorDataDir string

		writeTOC := func(bc *history.BackupConfig, entries []toc.CoordinatorDataEntry) {
			fpInfo, err := coordinatorFPInfo(coordinatorDataDir, bc)
			Expect(err).To(BeNil())
			dir := fpInfo.GetDirForContent(-1)
			Expect(os.MkdirAll(dir, 0755)).To(Succeed())

			contents, err := yaml.Marshal(&toc.TOC{DataEntries: entries})
			Expect(err).To(BeNil())
			Expect(os.WriteFile(fpInfo.GetTOCFilePath(), contents, 0644)).To(Succeed())
		}

		BeforeEach(func() {
			tmpDir, err := os.MkdirTemp("", "find_table_test")
			Expect(err).To(BeNil())
			coordinatorDataDir = tmpDir
			historyDBPath = path.Join(coordinatorDataDir, "gpbackup_history.db")
		})

		AfterEach(func() {
			_ = os.RemoveAll(coordinatorDataDir)
		})

		It("finds a table in a matching backup while skipping others that don't qualify", func() {
			db, err := history.InitializeHistoryDatabase(historyDBPath)
			Expect(err).To(BeNil())
			defer db.Close()

			matching := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260101000000"}
			noTable := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260102000000"}
			metadataOnly := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, MetadataOnly: true, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260103000000"}
			failed := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusFailed, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260104000000"}
			deleted := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, DateDeleted: "20260105000000", RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260105000000"}

			for _, bc := range []*history.BackupConfig{&matching, &noTable, &metadataOnly, &failed, &deleted} {
				Expect(history.StoreBackupHistory(db, bc)).To(Succeed())
			}

			writeTOC(&matching, []toc.CoordinatorDataEntry{{Schema: "public", Name: "foo"}})
			writeTOC(&noTable, []toc.CoordinatorDataEntry{{Schema: "public", Name: "bar"}})
			writeTOC(&deleted, []toc.CoordinatorDataEntry{{Schema: "public", Name: "foo"}})
			// metadataOnly and failed are never even looked up on disk, so their TOC is left unwritten.

			matches, _, err := findBackupsContainingTable(db, coordinatorDataDir, "public", "foo")
			Expect(err).To(BeNil())
			Expect(matches).To(HaveLen(1))
			Expect(matches[0].Timestamp).To(Equal(matching.Timestamp))
		})

		It("skips (rather than fails) a qualifying backup whose local TOC file is missing", func() {
			db, err := history.InitializeHistoryDatabase(historyDBPath)
			Expect(err).To(BeNil())
			defer db.Close()

			missingTOC := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260101000000"}
			Expect(history.StoreBackupHistory(db, &missingTOC)).To(Succeed())

			matches, warnings, err := findBackupsContainingTable(db, coordinatorDataDir, "public", "foo")
			Expect(err).To(BeNil())
			Expect(matches).To(BeEmpty())
			Expect(warnings).To(HaveLen(1))
		})

		It("returns nothing when no backups exist", func() {
			db, err := history.InitializeHistoryDatabase(historyDBPath)
			Expect(err).To(BeNil())
			defer db.Close()

			matches, _, err := findBackupsContainingTable(db, coordinatorDataDir, "public", "foo")
			Expect(err).To(BeNil())
			Expect(matches).To(BeEmpty())
		})
	})

	Describe("DoFindTable", func() {
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

		AfterEach(func() {
			operating.System = operating.InitializeSystemFunctions()
		})

		Context("--format=json", func() {
			It("prints an empty JSON array instead of crashing when no history db exists", func() {
				useFindTableFlags(map[string]string{options.FORMAT: "json"})

				var output string
				Expect(func() {
					output = captureStdout(func() { DoFindTable("public.foo") })
				}).ToNot(Panic())

				Expect(strings.TrimSpace(output)).To(Equal("[]"))
			})

			It("prints a matching backup as JSON", func() {
				historyDBPath := filepath.Join(tmpDir, "gpbackup_history.db")
				db, err := history.InitializeHistoryDatabase(historyDBPath)
				Expect(err).ToNot(HaveOccurred())

				matching := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260101000000"}
				Expect(history.StoreBackupHistory(db, &matching)).To(Succeed())
				Expect(db.Close()).To(Succeed())

				fpInfo, err := coordinatorFPInfo(tmpDir, &matching)
				Expect(err).ToNot(HaveOccurred())
				Expect(os.MkdirAll(fpInfo.GetDirForContent(-1), 0755)).To(Succeed())
				contents, err := yaml.Marshal(&toc.TOC{DataEntries: []toc.CoordinatorDataEntry{{Schema: "public", Name: "foo"}}})
				Expect(err).ToNot(HaveOccurred())
				Expect(os.WriteFile(fpInfo.GetTOCFilePath(), contents, 0644)).To(Succeed())

				useFindTableFlags(map[string]string{options.FORMAT: "json"})
				output := captureStdout(func() { DoFindTable("public.foo") })

				var entries []backupListEntry
				Expect(json.Unmarshal([]byte(output), &entries)).To(Succeed())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].Timestamp).To(Equal(matching.Timestamp))
			})

			It("routes a skip warning to stderr, leaving stdout as valid JSON", func() {
				historyDBPath := filepath.Join(tmpDir, "gpbackup_history.db")
				db, err := history.InitializeHistoryDatabase(historyDBPath)
				Expect(err).ToNot(HaveOccurred())

				missingTOC := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260101000000"}
				Expect(history.StoreBackupHistory(db, &missingTOC)).To(Succeed())
				Expect(db.Close()).To(Succeed())

				useFindTableFlags(map[string]string{options.FORMAT: "json"})

				var stdout string
				stderr := captureStderr(func() {
					stdout = captureStdout(func() { DoFindTable("public.foo") })
				})

				Expect(strings.TrimSpace(stdout)).To(Equal("[]"))
				Expect(stderr).To(ContainSubstring("WARNING"))
				Expect(stderr).To(ContainSubstring("Skipping backup"))
			})
		})

		Context("--format=text", func() {
			It("does not write a skip warning to stderr", func() {
				historyDBPath := filepath.Join(tmpDir, "gpbackup_history.db")
				db, err := history.InitializeHistoryDatabase(historyDBPath)
				Expect(err).ToNot(HaveOccurred())

				missingTOC := history.BackupConfig{DatabaseName: "testdb", Status: history.BackupStatusSucceed, RestorePlan: []history.RestorePlanEntry{}, Timestamp: "20260101000000"}
				Expect(history.StoreBackupHistory(db, &missingTOC)).To(Succeed())
				Expect(db.Close()).To(Succeed())

				useFindTableFlags(map[string]string{})

				stderr := captureStderr(func() {
					captureStdout(func() { DoFindTable("public.foo") })
				})

				Expect(stderr).To(BeEmpty())
			})
		})
	})
})
