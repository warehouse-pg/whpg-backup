package end_to_end_test

import (
	"fmt"
	"os"
	"os/exec"
	path "path/filepath"

	"github.com/greenplum-db/gpbackup/cluster"
	"github.com/greenplum-db/gpbackup/dbconn"
	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/iohelper"
	"github.com/greenplum-db/gpbackup/testhelper"
	"github.com/greenplum-db/gpbackup/testutils"
	"github.com/greenplum-db/gpbackup/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// patchConfigPluginField rewrites the backup config yaml emitted by gpbackup
// so the recorded plugin field is set to fakePluginPath, simulating a backup
// originally produced via a plugin (e.g. gpbackup_ddboost_plugin) without
// requiring the plugin to actually be installed. Goes through the history
// package so the yaml stays canonical (single plugin: key, etc.).
func patchConfigPluginField(backupDir, timestamp, fakePluginPath string) string {
	pattern := path.Join(backupDir, "*-1", "backups", timestamp[:8], timestamp,
		fmt.Sprintf("gpbackup_%s_config.yaml", timestamp))
	matches, err := path.Glob(pattern)
	Expect(err).ToNot(HaveOccurred())
	if len(matches) == 0 {
		matches, err = path.Glob(path.Join(backupDir, "backups", timestamp[:8], timestamp,
			fmt.Sprintf("gpbackup_%s_config.yaml", timestamp)))
		Expect(err).ToNot(HaveOccurred())
	}
	Expect(matches).To(HaveLen(1))
	configPath := matches[0]

	cfg := history.ReadConfigFile(configPath)
	cfg.Plugin = fakePluginPath
	// gpbackup leaves the config file read-only (0444); WriteConfigFile
	// silently swallows the resulting EACCES on overwrite. Restore write
	// permission first so the patch actually lands on disk.
	Expect(os.Chmod(configPath, 0644)).To(Succeed())
	history.WriteConfigFile(cfg, configPath)
	return configPath
}

func copyPluginToAllHosts(conn *dbconn.DBConn, pluginPath string) {
	hostnameQuery := `SELECT DISTINCT hostname AS string FROM gp_segment_configuration WHERE content != -1`
	hostnames := dbconn.MustSelectStringSlice(conn, hostnameQuery)
	for _, hostname := range hostnames {
		// Skip the local host
		h, _ := os.Hostname()
		if hostname == h {
			continue
		}
		examplePluginTestDir, _ := path.Split(pluginPath)
		command := exec.Command("ssh", hostname, fmt.Sprintf("mkdir -p %s", examplePluginTestDir))
		mustRunCommand(command)
		command = exec.Command("scp", pluginPath, fmt.Sprintf("%s:%s", hostname, pluginPath))
		mustRunCommand(command)
	}
}

func forceMetadataFileDownloadFromPlugin(conn *dbconn.DBConn, timestamp string) {
	fpInfo := filepath.NewFilePathInfo(backupCluster, "", timestamp, "", false)
	remoteOutput := backupCluster.GenerateAndExecuteCommand(
		fmt.Sprintf("Removing backups on all segments for "+
			"timestamp %s", timestamp),
		cluster.ON_SEGMENTS|cluster.INCLUDE_COORDINATOR,
		func(contentID int) string {
			return fmt.Sprintf("rm -rf %s", fpInfo.GetDirForContent(contentID))
		})
	if remoteOutput.NumErrors != 0 {
		Fail(fmt.Sprintf("Failed to remove backup directory for timestamp %s", timestamp))
	}
}

var _ = Describe("End to End plugin tests", func() {
	BeforeEach(func() {
		end_to_end_setup()
	})
	AfterEach(func() {
		end_to_end_teardown()
	})

	Describe("Single data file", func() {
		It("runs gpbackup and gprestore with single-data-file flag", func() {
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--single-data-file",
				"--backup-dir", backupDir)
			timestamp := getBackupTimestamp(string(output))

			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--backup-dir", backupDir)

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
			assertArtifactsCleaned(timestamp)

		})
		It("runs gpbackup and gprestore with single-data-file flag with copy-queue-size", func() {
			skipIfOldBackupVersionBefore("1.23.0")
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--single-data-file",
				"--copy-queue-size", "4",
				"--backup-dir", backupDir)
			timestamp := getBackupTimestamp(string(output))
			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--copy-queue-size", "4",
				"--backup-dir", backupDir)

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
			assertArtifactsCleaned(timestamp)

		})
		It("runs gpbackup and gprestore with single-data-file flag without compression", func() {
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--single-data-file",
				"--backup-dir", backupDir,
				"--no-compression")
			timestamp := getBackupTimestamp(string(output))
			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--backup-dir", backupDir)

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
			assertArtifactsCleaned(timestamp)
		})
		It("runs gpbackup and gprestore with single-data-file flag without compression with copy-queue-size", func() {
			skipIfOldBackupVersionBefore("1.23.0")
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--single-data-file",
				"--copy-queue-size", "4",
				"--backup-dir", backupDir,
				"--no-compression")
			timestamp := getBackupTimestamp(string(output))
			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--copy-queue-size", "4",
				"--backup-dir", backupDir)

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
			assertArtifactsCleaned(timestamp)
		})
		It("runs gpbackup and gprestore on database with all objects", func() {
			schemaResetStatements := "DROP SCHEMA IF EXISTS schema2 CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
			testhelper.AssertQueryRuns(backupConn, schemaResetStatements)
			defer testutils.ExecuteSQLFile(backupConn,
				"resources/test_tables_data.sql")
			defer testutils.ExecuteSQLFile(backupConn,
				"resources/test_tables_ddl.sql")
			defer testhelper.AssertQueryRuns(backupConn, schemaResetStatements)
			defer testhelper.AssertQueryRuns(restoreConn, schemaResetStatements)
			testhelper.AssertQueryRuns(backupConn,
				"CREATE ROLE testrole SUPERUSER")
			defer testhelper.AssertQueryRuns(backupConn,
				"DROP ROLE testrole")

			// In GPDB 7+, we use plpython3u because of default python 3 support.
			plpythonDropStatement := "DROP PROCEDURAL LANGUAGE IF EXISTS plpythonu;"
			if backupConn.Version.AtLeast("7") {
				plpythonDropStatement = "DROP PROCEDURAL LANGUAGE IF EXISTS plpython3u;"
			}
			testhelper.AssertQueryRuns(backupConn, plpythonDropStatement)
			defer testhelper.AssertQueryRuns(backupConn, plpythonDropStatement)
			defer testhelper.AssertQueryRuns(restoreConn, plpythonDropStatement)

			testutils.ExecuteSQLFile(backupConn, "resources/gpdb4_objects.sql")
			if backupConn.Version.Before("7") {
				testutils.ExecuteSQLFile(backupConn, "resources/gpdb4_compatible_objects_before_gpdb7.sql")
			} else {
				testutils.ExecuteSQLFile(backupConn, "resources/gpdb4_compatible_objects_after_gpdb7.sql")
			}

			if backupConn.Version.AtLeast("5") {
				testutils.ExecuteSQLFile(backupConn, "resources/gpdb5_objects.sql")
			}
			if backupConn.Version.AtLeast("6") {
				testutils.ExecuteSQLFile(backupConn, "resources/gpdb6_objects.sql")
				defer testhelper.AssertQueryRuns(backupConn,
					"DROP FOREIGN DATA WRAPPER fdw CASCADE;")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP FOREIGN DATA WRAPPER fdw CASCADE;")
			}
			if backupConn.Version.AtLeast("6.2") {
				testhelper.AssertQueryRuns(backupConn,
					"CREATE TABLE mview_table1(i int, j text);")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP TABLE mview_table1;")
				testhelper.AssertQueryRuns(backupConn,
					"CREATE MATERIALIZED VIEW mview1 (i2) as select i from mview_table1;")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP MATERIALIZED VIEW mview1;")
				testhelper.AssertQueryRuns(backupConn,
					"CREATE MATERIALIZED VIEW mview2 as select * from mview1;")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP MATERIALIZED VIEW mview2;")
			}

			if backupConn.Version.AtLeast("7") {
				testutils.ExecuteSQLFile(backupConn, "resources/external_partition_test.sql")
			}

			output := gpbackup(gpbackupPath, backupHelperPath,
				"--leaf-partition-data",
				"--single-data-file")
			timestamp := getBackupTimestamp(string(output))
			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--metadata-only",
				"--redirect-db", "restoredb")
			assertArtifactsCleaned(timestamp)
		})
		It("runs gpbackup and gprestore on database with all objects with copy-queue-size", func() {
			skipIfOldBackupVersionBefore("1.23.0")
			testhelper.AssertQueryRuns(backupConn,
				"DROP SCHEMA IF EXISTS schema2 CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP PROCEDURAL LANGUAGE IF EXISTS plpythonu;")
			defer testutils.ExecuteSQLFile(backupConn,
				"resources/test_tables_data.sql")
			defer testutils.ExecuteSQLFile(backupConn,
				"resources/test_tables_ddl.sql")
			defer testhelper.AssertQueryRuns(backupConn,
				"DROP SCHEMA IF EXISTS schema2 CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP PROCEDURAL LANGUAGE IF EXISTS plpythonu;")
			defer testhelper.AssertQueryRuns(restoreConn,
				"DROP SCHEMA IF EXISTS schema2 CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP PROCEDURAL LANGUAGE IF EXISTS plpythonu;")
			testhelper.AssertQueryRuns(backupConn,
				"CREATE ROLE testrole SUPERUSER")
			defer testhelper.AssertQueryRuns(backupConn,
				"DROP ROLE testrole")
			testutils.ExecuteSQLFile(backupConn, "resources/gpdb4_objects.sql")
			if backupConn.Version.AtLeast("5") {
				testutils.ExecuteSQLFile(backupConn, "resources/gpdb5_objects.sql")
			}
			if backupConn.Version.AtLeast("6") {
				testutils.ExecuteSQLFile(backupConn, "resources/gpdb6_objects.sql")
				defer testhelper.AssertQueryRuns(backupConn,
					"DROP FOREIGN DATA WRAPPER fdw CASCADE;")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP FOREIGN DATA WRAPPER fdw CASCADE;")
			}
			if backupConn.Version.AtLeast("6.2") {
				testhelper.AssertQueryRuns(backupConn,
					"CREATE TABLE mview_table1(i int, j text);")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP TABLE mview_table1;")
				testhelper.AssertQueryRuns(backupConn,
					"CREATE MATERIALIZED VIEW mview1 (i2) as select i from mview_table1;")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP MATERIALIZED VIEW mview1;")
				testhelper.AssertQueryRuns(backupConn,
					"CREATE MATERIALIZED VIEW mview2 as select * from mview1;")
				defer testhelper.AssertQueryRuns(restoreConn,
					"DROP MATERIALIZED VIEW mview2;")
			}

			output := gpbackup(gpbackupPath, backupHelperPath,
				"--leaf-partition-data",
				"--single-data-file",
				"--copy-queue-size", "4")
			timestamp := getBackupTimestamp(string(output))
			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--metadata-only",
				"--redirect-db", "restoredb",
				"--copy-queue-size", "4")
			assertArtifactsCleaned(timestamp)
		})

		Context("with include filtering on restore", func() {
			It("runs gpbackup and gprestore with include-table-file restore flag with a single data file", func() {
				includeFile := iohelper.MustOpenFileForWriting("/tmp/include-tables.txt")
				utils.MustPrintln(includeFile, "public.sales\npublic.foo\npublic.myseq1\npublic.myview1")
				output := gpbackup(gpbackupPath, backupHelperPath,
					"--backup-dir", backupDir,
					"--single-data-file")
				timestamp := getBackupTimestamp(string(output))
				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--backup-dir", backupDir,
					"--include-table-file", "/tmp/include-tables.txt")
				assertRelationsCreated(restoreConn, 16)
				assertDataRestored(restoreConn, map[string]int{
					"public.sales": 13, "public.foo": 40000})
				assertArtifactsCleaned(timestamp)

				_ = os.Remove("/tmp/include-tables.txt")
			})
			It("runs gpbackup and gprestore with include-table-file restore flag with a single data with copy-queue-size", func() {
				skipIfOldBackupVersionBefore("1.23.0")
				includeFile := iohelper.MustOpenFileForWriting("/tmp/include-tables.txt")
				utils.MustPrintln(includeFile, "public.sales\npublic.foo\npublic.myseq1\npublic.myview1")
				output := gpbackup(gpbackupPath, backupHelperPath,
					"--backup-dir", backupDir,
					"--single-data-file",
					"--copy-queue-size", "4")
				timestamp := getBackupTimestamp(string(output))
				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--backup-dir", backupDir,
					"--include-table-file", "/tmp/include-tables.txt",
					"--copy-queue-size", "4")
				assertRelationsCreated(restoreConn, 16)
				assertDataRestored(restoreConn, map[string]int{
					"public.sales": 13, "public.foo": 40000})
				assertArtifactsCleaned(timestamp)

				_ = os.Remove("/tmp/include-tables.txt")
			})
			It("runs gpbackup and gprestore with include-schema restore flag with a single data file", func() {
				output := gpbackup(gpbackupPath, backupHelperPath,
					"--backup-dir", backupDir,
					"--single-data-file")
				timestamp := getBackupTimestamp(string(output))
				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--backup-dir", backupDir,
					"--include-schema", "schema2")

				assertRelationsCreated(restoreConn, 17)
				assertDataRestored(restoreConn, schema2TupleCounts)
				assertArtifactsCleaned(timestamp)
			})
			It("runs gpbackup and gprestore with include-schema restore flag with a single data file with copy-queue-size", func() {
				skipIfOldBackupVersionBefore("1.23.0")
				output := gpbackup(gpbackupPath, backupHelperPath,
					"--backup-dir", backupDir,
					"--single-data-file",
					"--copy-queue-size", "4")
				timestamp := getBackupTimestamp(string(output))
				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--backup-dir", backupDir,
					"--include-schema", "schema2",
					"--copy-queue-size", "4")

				assertRelationsCreated(restoreConn, 17)
				assertDataRestored(restoreConn, schema2TupleCounts)
				assertArtifactsCleaned(timestamp)
			})
		})

		Context("with plugin", func() {
			BeforeEach(func() {
				skipIfOldBackupVersionBefore("1.7.0")
				// FIXME: we are temporarily disabling these tests because we will be altering our backwards compatibility logic.
				if useOldBackupVersion {
					Skip("This test is only needed for the most recent backup versions")
				}
			})
			It("runs gpbackup and gprestore with plugin, single-data-file, and no-compression", func() {
				copyPluginToAllHosts(backupConn, examplePluginExec)

				output := gpbackup(gpbackupPath, backupHelperPath,
					"--single-data-file",
					"--no-compression",
					"--plugin-config", examplePluginTestConfig)
				timestamp := getBackupTimestamp(string(output))
				forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--plugin-config", examplePluginTestConfig)

				assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)
				assertArtifactsCleaned(timestamp)
			})
			It("runs gpbackup and gprestore with plugin, single-data-file, no-compression, and copy-queue-size", func() {
				copyPluginToAllHosts(backupConn, examplePluginExec)

				output := gpbackup(gpbackupPath, backupHelperPath,
					"--single-data-file",
					"--copy-queue-size", "4",
					"--no-compression",
					"--plugin-config", examplePluginTestConfig)
				timestamp := getBackupTimestamp(string(output))
				forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--plugin-config", examplePluginTestConfig,
					"--copy-queue-size", "4")

				assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)
				assertArtifactsCleaned(timestamp)
			})
			It("runs gpbackup and gprestore with plugin and single-data-file", func() {
				copyPluginToAllHosts(backupConn, examplePluginExec)

				output := gpbackup(gpbackupPath, backupHelperPath,
					"--single-data-file",
					"--plugin-config", examplePluginTestConfig)
				timestamp := getBackupTimestamp(string(output))
				forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--plugin-config", examplePluginTestConfig)

				assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)
				assertArtifactsCleaned(timestamp)
			})
			It("runs gpbackup and gprestore with plugin, single-data-file, and copy-queue-size", func() {
				copyPluginToAllHosts(backupConn, examplePluginExec)

				output := gpbackup(gpbackupPath, backupHelperPath,
					"--single-data-file",
					"--copy-queue-size", "4",
					"--plugin-config", examplePluginTestConfig)
				timestamp := getBackupTimestamp(string(output))
				forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--plugin-config", examplePluginTestConfig,
					"--copy-queue-size", "4")

				assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)
				assertArtifactsCleaned(timestamp)
			})
			It("runs gpbackup and gprestore with plugin and metadata-only", func() {
				copyPluginToAllHosts(backupConn, examplePluginExec)

				output := gpbackup(gpbackupPath, backupHelperPath,
					"--metadata-only",
					"--plugin-config", examplePluginTestConfig)
				timestamp := getBackupTimestamp(string(output))
				forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

				gprestore(gprestorePath, restoreHelperPath, timestamp,
					"--redirect-db", "restoredb",
					"--plugin-config", examplePluginTestConfig)

				assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
				assertArtifactsCleaned(timestamp)
			})
		})
	})

	Describe("Multi-file Plugin", func() {
		It("runs gpbackup and gprestore with plugin and no-compression", func() {
			skipIfOldBackupVersionBefore("1.7.0")
			// FIXME: we are temporarily disabling these tests because we will be altering our backwards compatibility logic.
			if useOldBackupVersion {
				Skip("This test is only needed for the most recent backup versions")
			}
			copyPluginToAllHosts(backupConn, examplePluginExec)

			output := gpbackup(gpbackupPath, backupHelperPath,
				"--no-compression",
				"--plugin-config", examplePluginTestConfig)
			timestamp := getBackupTimestamp(string(output))
			forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--plugin-config", examplePluginTestConfig)

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
		})
		It("runs gpbackup and gprestore with plugin and compression", func() {
			skipIfOldBackupVersionBefore("1.7.0")
			// FIXME: we are temporarily disabling these tests because we will be altering our backwards compatibility logic.
			if useOldBackupVersion {
				Skip("This test is only needed for the most recent backup versions")
			}
			copyPluginToAllHosts(backupConn, examplePluginExec)

			output := gpbackup(gpbackupPath, backupHelperPath,
				"--plugin-config", examplePluginTestConfig)
			timestamp := getBackupTimestamp(string(output))
			forceMetadataFileDownloadFromPlugin(backupConn, timestamp)

			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--plugin-config", examplePluginTestConfig)

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
		})
	})

	Describe("Example Plugin", func() {
		It("runs example_plugin.bash with plugin_test", func() {
			if useOldBackupVersion {
				Skip("This test is only needed for the latest backup version")
			}
			copyPluginToAllHosts(backupConn, examplePluginExec)
			command := exec.Command("bash", "-c", fmt.Sprintf("%s/plugin_test.sh %s %s", examplePluginDir, examplePluginExec, examplePluginTestConfig))
			mustRunCommand(command)
		})
	})

	Describe("--ignore-plugin-config", func() {
		// These tests cover the scenario where a backup was originally taken
		// with a plugin (e.g. ddboost) but the resulting files are reachable
		// on a regular filesystem (e.g. via a BoostFS mount). The recorded
		// plugin name is forced into the config yaml to mimic this without
		// requiring a real plugin to be installed.
		const fakePluginPath = "/tmp/gpbackup_fake_plugin"
		var timestamp string

		BeforeEach(func() {
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--backup-dir", backupDir)
			timestamp = getBackupTimestamp(string(output))
		})

		It("restores a backup that was taken with a plugin when --ignore-plugin-config is set", func() {
			patchConfigPluginField(backupDir, timestamp, fakePluginPath)

			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--backup-dir", backupDir,
				"--ignore-plugin-config")

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
			assertArtifactsCleaned(timestamp)
		})

		It("fails to restore a backup that was taken with a plugin without --ignore-plugin-config or --plugin-config", func() {
			patchConfigPluginField(backupDir, timestamp, fakePluginPath)

			cmd := exec.Command(gprestorePath,
				"--verbose",
				"--timestamp", timestamp,
				"--redirect-db", "restoredb",
				"--backup-dir", backupDir)
			out, err := cmd.CombinedOutput()
			Expect(err).To(HaveOccurred())
			Expect(string(out)).To(ContainSubstring(
				fmt.Sprintf("Backup was taken with plugin %s. The --plugin-config flag must be used to restore.", fakePluginPath)))
		})

		It("rejects --ignore-plugin-config combined with --plugin-config", func() {
			// --backup-dir is intentionally omitted: it is also mutually
			// exclusive with --plugin-config, and that check fires first.
			cmd := exec.Command(gprestorePath,
				"--verbose",
				"--timestamp", timestamp,
				"--redirect-db", "restoredb",
				"--plugin-config", examplePluginTestConfig,
				"--ignore-plugin-config")
			out, err := cmd.CombinedOutput()
			Expect(err).To(HaveOccurred())
			Expect(string(out)).To(ContainSubstring("plugin-config, ignore-plugin-config"))
		})

		It("is a no-op when --ignore-plugin-config is used on a backup taken without a plugin", func() {
			gprestore(gprestorePath, restoreHelperPath, timestamp,
				"--redirect-db", "restoredb",
				"--backup-dir", backupDir,
				"--ignore-plugin-config")

			assertRelationsCreated(restoreConn, TOTAL_RELATIONS)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
			assertArtifactsCleaned(timestamp)
		})
	})
})
