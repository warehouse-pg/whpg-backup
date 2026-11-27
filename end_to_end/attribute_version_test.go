package end_to_end_test

import (
	"fmt"
	"os"
	"os/exec"
	path "path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/testhelper"
)

var _ = Describe("attribute version option end to end tests", func() {
	BeforeEach(func() {
		end_to_end_setup()
	})
	AfterEach(func() {
		end_to_end_teardown()
	})

	It("restores a backup that contains the internal attribute version option", func() {
		// A gpbackup that knows the option never writes it, so a backup taken
		// by one that did not is made here: user options are replaced, in
		// the metadata file, by text of the same length, which keeps the
		// offsets in the table of contents valid.
		userOption := "n_distinct=1234567"
		internalOption := "pg_attver=12345678"
		Expect(internalOption).To(HaveLen(len(userOption)))

		testhelper.AssertQueryRuns(backupConn, "DROP TABLE IF EXISTS public.attver_e2e")
		testhelper.AssertQueryRuns(restoreConn, "DROP TABLE IF EXISTS public.attver_e2e")
		defer func() {
			backupConn.Exec("DROP TABLE IF EXISTS public.attver_e2e")
			restoreConn.Exec("DROP TABLE IF EXISTS public.attver_e2e")
		}()
		testhelper.AssertQueryRuns(backupConn, "CREATE TABLE public.attver_e2e (i int, j text, k text) DISTRIBUTED BY (i)")
		testhelper.AssertQueryRuns(backupConn, fmt.Sprintf("ALTER TABLE ONLY public.attver_e2e ALTER COLUMN j SET (%s)", userOption))
		testhelper.AssertQueryRuns(backupConn, fmt.Sprintf("ALTER TABLE ONLY public.attver_e2e ALTER COLUMN k SET (%s, n_distinct_inherited=5)", userOption))

		output := gpbackup(gpbackupPath, backupHelperPath,
			"--backup-dir", backupDir,
			"--metadata-only",
			"--include-table", "public.attver_e2e")
		timestamp := getBackupTimestamp(string(output))

		metadataFilePath := path.Join("backups", timestamp[:8], timestamp, fmt.Sprintf("gpbackup_%s_metadata.sql", timestamp))
		if _, err := os.Stat(path.Join(backupDir, "backups")); err != nil && os.IsNotExist(err) {
			metadataFilePath = path.Join("*-1", metadataFilePath)
		}
		metadataFiles, err := path.Glob(path.Join(backupDir, metadataFilePath))
		Expect(err).ToNot(HaveOccurred())
		Expect(metadataFiles).To(HaveLen(1))
		contents, err := os.ReadFile(metadataFiles[0])
		Expect(err).ToNot(HaveOccurred())
		Expect(strings.Count(string(contents), userOption)).To(Equal(2))
		Expect(os.Chmod(metadataFiles[0], 0644)).To(Succeed())
		Expect(os.WriteFile(metadataFiles[0], []byte(strings.ReplaceAll(string(contents), userOption, internalOption)), 0644)).To(Succeed())

		// A server without attribute versioning rejects the option and a
		// server with it ignores the option, so that the restore succeeds
		// does not tell on every server whether gprestore removed it. The
		// statements gprestore ran do, and it logs them with --debug. The
		// gprestore() helper passes --verbose, which excludes --debug.
		restoreOutput := mustRunCommand(exec.Command(gprestorePath,
			"--debug",
			"--timestamp", timestamp,
			"--redirect-db", "restoredb",
			"--backup-dir", backupDir))
		Expect(string(restoreOutput)).To(ContainSubstring("CREATE TABLE public.attver_e2e"))
		Expect(string(restoreOutput)).ToNot(ContainSubstring(internalOption))

		assertTablesRestored(restoreConn, []string{"public.attver_e2e"})
		userOptions := func(column string) string {
			return dbconn.MustSelectString(restoreConn, fmt.Sprintf(`SELECT coalesce(array_to_string(ARRAY(
					SELECT o FROM unnest(attoptions) o WHERE o NOT LIKE 'pg_attver=%%'), ','), '') AS string
				FROM pg_attribute WHERE attrelid = 'public.attver_e2e'::regclass AND attname = '%s'`, column))
		}
		Expect(userOptions("j")).To(Equal(""))
		Expect(userOptions("k")).To(Equal("n_distinct_inherited=5"))
	})
})
