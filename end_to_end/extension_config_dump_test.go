package end_to_end_test

import (
	"fmt"

	"github.com/greenplum-db/gp-common-go-libs/dbconn"
	"github.com/greenplum-db/gp-common-go-libs/testhelper"
	"github.com/greenplum-db/gpbackup/toc"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v2"
)

var _ = Describe("extension config dump end to end tests", func() {
	BeforeEach(func() {
		end_to_end_setup()
	})
	AfterEach(func() {
		end_to_end_teardown()
	})

	It("backs up and restores data for extension config dump tables", func() {
		// This test uses the repository's test extension (test_extension_config_dump). The extension's install
		// script creates a config table and registers it via pg_extension_config_dump().
		// With this PR, gpbackup should include that table in data backups and gprestore should
		// COPY the data back into the table after CREATE EXTENSION runs.

		// Ensure a clean slate in both DBs.
		backupConn.Exec("DROP EXTENSION IF EXISTS test_extension_config_dump CASCADE")
		restoreConn.Exec("DROP EXTENSION IF EXISTS test_extension_config_dump CASCADE")
		testhelper.AssertQueryRuns(backupConn, "DROP SCHEMA IF EXISTS extcfg CASCADE")
		testhelper.AssertQueryRuns(restoreConn, "DROP SCHEMA IF EXISTS extcfg CASCADE")

		// Always clean up even if gpbackup/gprestore fails mid-test.
		defer func() {
			backupConn.Exec("DROP EXTENSION IF EXISTS test_extension_config_dump CASCADE")
			backupConn.Exec("DROP SCHEMA IF EXISTS extcfg CASCADE")
			restoreConn.Exec("DROP EXTENSION IF EXISTS test_extension_config_dump CASCADE")
			restoreConn.Exec("DROP SCHEMA IF EXISTS extcfg CASCADE")
		}()

		testhelper.AssertQueryRuns(backupConn, "CREATE SCHEMA extcfg")
		// Install extension into extcfg so it doesn't interfere with other tests' public/schema2 counts.
		testhelper.AssertQueryRuns(backupConn, "CREATE EXTENSION test_extension_config_dump WITH SCHEMA extcfg")

		// Insert user configuration data to be preserved by the backup.
		testhelper.AssertQueryRuns(backupConn, "INSERT INTO extcfg.test_extension_config_dump_config VALUES (1, 'a'), (2, 'b')")

		output := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupDir)
		timestamp := getBackupTimestamp(string(output))
		Expect(timestamp).ToNot(BeEmpty())

		// Assert the extension config dump table is present in toc.yaml data entries.
		tocFileContents := getMetdataFileContents(backupDir, timestamp, "toc.yaml")
		tocStruct := &toc.TOC{}
		err := yaml.Unmarshal(tocFileContents, tocStruct)
		Expect(err).ToNot(HaveOccurred())
		found := false
		for _, entry := range tocStruct.DataEntries {
			if entry.Schema == "extcfg" && entry.Name == "test_extension_config_dump_config" {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "expected extcfg.test_extension_config_dump_config to appear in toc.yaml data entries")

		// Sanity: metadata.sql should not contain the table DDL (extension manages DDL).
		metadataFileContents := string(getMetdataFileContents(backupDir, timestamp, "metadata.sql"))
		Expect(metadataFileContents).ToNot(ContainSubstring("test_extension_config_dump_config"))

		// Restore. CREATE SCHEMA extcfg and CREATE EXTENSION test_extension_config_dump (WITH SCHEMA extcfg)
		// should happen during predata, then data should be COPY'd in.
		_ = gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb",
			"--backup-dir", backupDir)

		Expect(dbconn.MustSelectString(restoreConn, "SELECT count(*) AS string FROM extcfg.test_extension_config_dump_config")).To(Equal("2"))
		Expect(dbconn.MustSelectString(restoreConn, fmt.Sprintf("SELECT val AS string FROM extcfg.test_extension_config_dump_config WHERE id = %d", 1))).To(Equal("a"))
		Expect(dbconn.MustSelectString(restoreConn, fmt.Sprintf("SELECT val AS string FROM extcfg.test_extension_config_dump_config WHERE id = %d", 2))).To(Equal("b"))
	})
})
