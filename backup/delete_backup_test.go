package backup

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/warehouse-pg/common-go-libs/cluster"
	"github.com/warehouse-pg/common-go-libs/testhelper"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("delete-backup internal tests", func() {
	Describe("resolveDeletionOrder", func() {
		var historyDBPath = "/tmp/delete_backup_test_history.db"
		var fullConfig, incrementalConfig history.BackupConfig

		BeforeEach(func() {
			fullConfig = history.BackupConfig{
				DatabaseName:     "testdb",
				ExcludeRelations: []string{},
				ExcludeSchemas:   []string{},
				IncludeRelations: []string{},
				IncludeSchemas:   []string{},
				RestorePlan:      []history.RestorePlanEntry{},
				Timestamp:        "20260101000000",
			}
			incrementalConfig = history.BackupConfig{
				DatabaseName:     "testdb",
				ExcludeRelations: []string{},
				ExcludeSchemas:   []string{},
				IncludeRelations: []string{},
				IncludeSchemas:   []string{},
				Incremental:      true,
				RestorePlan: []history.RestorePlanEntry{
					{Timestamp: "20260101000000", TableFQNs: []string{"public.foo"}},
					{Timestamp: "20260101010000", TableFQNs: []string{"public.foo"}},
				},
				Timestamp: "20260101010000",
			}
			_ = os.Remove(historyDBPath)
		})

		AfterEach(func() {
			_ = os.Remove(historyDBPath)
		})

		It("returns just the target when nothing depends on it", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			Expect(history.StoreBackupHistory(db, &fullConfig)).To(Succeed())

			order, err := resolveDeletionOrder(db, &fullConfig, false)
			Expect(err).To(BeNil())
			Expect(order).To(HaveLen(1))
			Expect(order[0].Timestamp).To(Equal(fullConfig.Timestamp))
		})

		It("blocks deletion when a live dependent exists and cascade is false", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			Expect(history.StoreBackupHistory(db, &fullConfig)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incrementalConfig)).To(Succeed())

			_, err := resolveDeletionOrder(db, &fullConfig, false)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(incrementalConfig.Timestamp))
			Expect(err.Error()).To(ContainSubstring("--cascade"))
		})

		It("includes the dependent, target last, when cascade is true", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			Expect(history.StoreBackupHistory(db, &fullConfig)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incrementalConfig)).To(Succeed())

			order, err := resolveDeletionOrder(db, &fullConfig, true)
			Expect(err).To(BeNil())
			Expect(order).To(HaveLen(2))
			Expect(order[0].Timestamp).To(Equal(incrementalConfig.Timestamp))
			Expect(order[len(order)-1].Timestamp).To(Equal(fullConfig.Timestamp))
		})

		It("does not block on a dependent that has already been deleted", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			Expect(history.StoreBackupHistory(db, &fullConfig)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incrementalConfig)).To(Succeed())
			Expect(history.SetDateDeleted(db, incrementalConfig.Timestamp, "20260102000000")).To(Succeed())

			order, err := resolveDeletionOrder(db, &fullConfig, false)
			Expect(err).To(BeNil())
			Expect(order).To(HaveLen(1))
			Expect(order[0].Timestamp).To(Equal(fullConfig.Timestamp))
		})

		It("blocks deletion when a dependent backup is still in progress, even with cascade", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			incrementalConfig.Status = history.BackupStatusInProgress
			Expect(history.StoreBackupHistory(db, &fullConfig)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incrementalConfig)).To(Succeed())

			_, err := resolveDeletionOrder(db, &fullConfig, true)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(incrementalConfig.Timestamp))
			Expect(err.Error()).To(ContainSubstring("in progress"))
		})

		It("orders a three-deep incremental chain newest-first, target last", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			incr2Config := history.BackupConfig{
				DatabaseName:     "testdb",
				ExcludeRelations: []string{},
				ExcludeSchemas:   []string{},
				IncludeRelations: []string{},
				IncludeSchemas:   []string{},
				Incremental:      true,
				RestorePlan: []history.RestorePlanEntry{
					{Timestamp: "20260101000000", TableFQNs: []string{"public.foo"}},
					{Timestamp: "20260101010000", TableFQNs: []string{"public.foo"}},
					{Timestamp: "20260101020000", TableFQNs: []string{"public.foo"}},
				},
				Timestamp: "20260101020000",
			}
			Expect(history.StoreBackupHistory(db, &fullConfig)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incrementalConfig)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incr2Config)).To(Succeed())

			order, err := resolveDeletionOrder(db, &fullConfig, true)
			Expect(err).To(BeNil())
			Expect(order).To(HaveLen(3))
			Expect(order[0].Timestamp).To(Equal(incr2Config.Timestamp))
			Expect(order[1].Timestamp).To(Equal(incrementalConfig.Timestamp))
			Expect(order[2].Timestamp).To(Equal(fullConfig.Timestamp))
		})
	})

	Describe("promptForDeletion", func() {
		var origStdin *os.File

		BeforeEach(func() {
			origStdin = os.Stdin
		})

		AfterEach(func() {
			os.Stdin = origStdin
		})

		respond := func(input string) bool {
			r, w, err := os.Pipe()
			Expect(err).To(BeNil())
			_, err = w.WriteString(input)
			Expect(err).To(BeNil())
			Expect(w.Close()).To(Succeed())
			os.Stdin = r
			return promptForDeletion([]*history.BackupConfig{{Timestamp: "20260101000000", DatabaseName: "testdb"}})
		}

		It("accepts y", func() {
			Expect(respond("y\n")).To(BeTrue())
		})
		It("accepts yes case-insensitively", func() {
			Expect(respond("YES\n")).To(BeTrue())
		})
		It("rejects n", func() {
			Expect(respond("n\n")).To(BeFalse())
		})
		It("rejects an empty line", func() {
			Expect(respond("\n")).To(BeFalse())
		})
		It("rejects unrecognized input", func() {
			Expect(respond("maybe\n")).To(BeFalse())
		})
	})

	Describe("deleteLocalBackupFiles", func() {
		var testCluster *cluster.Cluster
		var executor testhelper.TestExecutor
		var bc *history.BackupConfig

		BeforeEach(func() {
			testCluster = cluster.NewCluster([]cluster.SegConfig{
				{DbID: 1, ContentID: -1, Role: "p", DataDir: "/data/master", Hostname: "coordinator"},
				{DbID: 2, ContentID: 0, Role: "p", DataDir: "/data/seg0", Hostname: "seghost1"},
				{DbID: 3, ContentID: 0, Role: "m", DataDir: "/data/mirror0", Hostname: "mirrorhost1"},
				{DbID: 4, ContentID: 1, Role: "p", DataDir: "/data/seg1", Hostname: "seghost2"},
			})
			executor = testhelper.TestExecutor{ClusterOutput: &cluster.RemoteOutput{}}
			testCluster.Executor = &executor
			bc = &history.BackupConfig{Timestamp: "20260101000000"}
		})

		It("issues one rm -rf per segment (both primary and mirror), targeting the right host", func() {
			err := deleteLocalBackupFiles(testCluster, bc)
			Expect(err).To(BeNil())

			Expect(executor.ClusterCommands).To(HaveLen(1))
			commands := executor.ClusterCommands[0]
			Expect(commands).To(HaveLen(4))

			allCommandStrings := make([]string, len(commands))
			for i, cmd := range commands {
				allCommandStrings[i] = cmd.CommandString
			}
			joined := strings.Join(allCommandStrings, "\n")

			// Coordinator segment runs locally (no ssh, no hostname in the command).
			Expect(joined).To(ContainSubstring("bash -c rm -rf '/data/master/backups/20260101/20260101000000'"))
			// Every other segment/mirror runs over ssh to its own host.
			Expect(joined).To(MatchRegexp(`ssh .*seghost1.* rm -rf '/data/seg0/backups/20260101/20260101000000'`))
			Expect(joined).To(MatchRegexp(`ssh .*mirrorhost1.* rm -rf '/data/mirror0/backups/20260101/20260101000000'`))
			Expect(joined).To(MatchRegexp(`ssh .*seghost2.* rm -rf '/data/seg1/backups/20260101/20260101000000'`))
		})

		It("returns an error when any host fails to delete its files", func() {
			executor.ClusterOutput = &cluster.RemoteOutput{NumErrors: 1}

			err := deleteLocalBackupFiles(testCluster, bc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(bc.Timestamp))
		})

		It("issues only one rm -rf per (host, dir) when SingleBackupDir maps every content on a host to the same path", func() {
			multiSegHostCluster := cluster.NewCluster([]cluster.SegConfig{
				{DbID: 1, ContentID: -1, Role: "p", DataDir: "/data/master", Hostname: "coordinator"},
				{DbID: 2, ContentID: 0, Role: "p", DataDir: "/data/seg0", Hostname: "seghost1"},
				{DbID: 3, ContentID: 1, Role: "p", DataDir: "/data/seg1", Hostname: "seghost1"},
			})
			multiSegExecutor := testhelper.TestExecutor{ClusterOutput: &cluster.RemoteOutput{}}
			multiSegHostCluster.Executor = &multiSegExecutor
			bc.BackupDir = "/shared/backups"
			bc.SingleBackupDir = true

			err := deleteLocalBackupFiles(multiSegHostCluster, bc)
			Expect(err).To(BeNil())

			// Both segments on seghost1 resolve to the identical shared directory; only one
			// rm -rf should be issued for that (host, dir) pair, plus one for the coordinator.
			Expect(multiSegExecutor.ClusterCommands[0]).To(HaveLen(2))
		})

		It("returns an error instead of deleting a relative path when a segment's data dir is unknown", func() {
			unknownDirCluster := cluster.NewCluster([]cluster.SegConfig{
				{DbID: 1, ContentID: -1, Role: "p", DataDir: "/data/master", Hostname: "coordinator"},
				{DbID: 2, ContentID: 0, Role: "p", DataDir: "", Hostname: "seghost1"},
			})
			unknownExecutor := testhelper.TestExecutor{ClusterOutput: &cluster.RemoteOutput{}}
			unknownDirCluster.Executor = &unknownExecutor

			err := deleteLocalBackupFiles(unknownDirCluster, bc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("relative path"))
			// Must abort before running anything, not just skip the bad segment.
			Expect(unknownExecutor.ClusterCommands).To(HaveLen(0))
		})
	})

	Describe("deleteOneBackup", func() {
		var historyDBPath = "/tmp/delete_backup_test_history_one.db"
		var bc *history.BackupConfig

		BeforeEach(func() {
			bc = &history.BackupConfig{
				DatabaseName:     "testdb",
				ExcludeRelations: []string{},
				ExcludeSchemas:   []string{},
				IncludeRelations: []string{},
				IncludeSchemas:   []string{},
				RestorePlan:      []history.RestorePlanEntry{},
				Timestamp:        "20260101000000",
			}
			_ = os.Remove(historyDBPath)
		})

		AfterEach(func() {
			_ = os.Remove(historyDBPath)
		})

		writePluginScript := func(dir string, exitCode int) string {
			scriptPath := filepath.Join(dir, "fake_plugin.sh")
			script := "#!/bin/bash\n"
			if exitCode != 0 {
				script += "echo \"boom\" 1>&2\n"
			}
			script += fmt.Sprintf("exit %d\n", exitCode)
			Expect(ioutil.WriteFile(scriptPath, []byte(script), 0755)).To(Succeed())
			return scriptPath
		}

		It("marks the backup deleted with a completed timestamp on plugin success, and also removes local coordinator files", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			Expect(history.StoreBackupHistory(db, bc)).To(Succeed())

			tempDir, err := ioutil.TempDir("", "delete_backup_plugin")
			Expect(err).To(BeNil())
			defer os.RemoveAll(tempDir)

			// Simulate the coordinator-local metadata/report files backup.go always writes,
			// even for a plugin backup, which pluginConfig.DeleteBackup alone would never clean up.
			localBackupDir := filepath.Join(tempDir, "backups", bc.Timestamp[0:8], bc.Timestamp)
			Expect(os.MkdirAll(localBackupDir, 0755)).To(Succeed())
			Expect(ioutil.WriteFile(filepath.Join(localBackupDir, "metadata.sql"), []byte("-- fake"), 0644)).To(Succeed())

			pluginConfig := &utils.PluginConfig{ExecutablePath: writePluginScript(tempDir, 0), ConfigPath: "/tmp/my_plugin_config.yaml"}

			deleteOneBackup(db, bc, pluginConfig, nil, tempDir)

			updated, err := history.GetBackupConfig(bc.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(isFullyDeleted(updated.DateDeleted)).To(BeTrue())
			_, statErr := os.Stat(localBackupDir)
			Expect(os.IsNotExist(statErr)).To(BeTrue())
		})

		It("marks the backup as plugin-delete-failed and panics on plugin failure", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			Expect(history.StoreBackupHistory(db, bc)).To(Succeed())

			tempDir, err := ioutil.TempDir("", "delete_backup_plugin")
			Expect(err).To(BeNil())
			defer os.RemoveAll(tempDir)

			pluginConfig := &utils.PluginConfig{ExecutablePath: writePluginScript(tempDir, 1), ConfigPath: "/tmp/my_plugin_config.yaml"}

			func() {
				defer testhelper.ShouldPanicWithMessage("boom")
				deleteOneBackup(db, bc, pluginConfig, nil, tempDir)
			}()

			updated, err := history.GetBackupConfig(bc.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(updated.DateDeleted).To(Equal(deleteStatusPluginFailed))
		})

		It("marks the backup local-delete-failed and panics when the cluster rm fails", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			Expect(history.StoreBackupHistory(db, bc)).To(Succeed())

			testCluster := cluster.NewCluster([]cluster.SegConfig{
				{DbID: 1, ContentID: -1, Role: "p", DataDir: "/data/master", Hostname: "coordinator"},
			})
			executor := testhelper.TestExecutor{ClusterOutput: &cluster.RemoteOutput{NumErrors: 1}}
			testCluster.Executor = &executor

			func() {
				defer testhelper.ShouldPanicWithMessage(bc.Timestamp)
				deleteOneBackup(db, bc, nil, testCluster, "")
			}()

			updated, err := history.GetBackupConfig(bc.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(updated.DateDeleted).To(Equal(deleteStatusLocalFailed))
		})
	})

	Describe("validatePluginConfigMatch", func() {
		It("passes when a plugin backup is deleted with --plugin-config", func() {
			backups := []*history.BackupConfig{{Timestamp: "20260101000000", Plugin: "gpbackup_s3_plugin"}}
			Expect(validatePluginConfigMatch(backups, "/etc/s3_config.yaml")).To(BeNil())
		})

		It("passes when a non-plugin backup is deleted without --plugin-config", func() {
			backups := []*history.BackupConfig{{Timestamp: "20260101000000"}}
			Expect(validatePluginConfigMatch(backups, "")).To(BeNil())
		})

		It("fails when a plugin backup is deleted without --plugin-config", func() {
			backups := []*history.BackupConfig{{Timestamp: "20260101000000", Plugin: "gpbackup_s3_plugin"}}
			err := validatePluginConfigMatch(backups, "")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("20260101000000"))
			Expect(err.Error()).To(ContainSubstring("--plugin-config"))
		})

		It("fails when a non-plugin backup is deleted with --plugin-config", func() {
			backups := []*history.BackupConfig{{Timestamp: "20260101000000"}}
			err := validatePluginConfigMatch(backups, "/etc/s3_config.yaml")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("20260101000000"))
			Expect(err.Error()).To(ContainSubstring("not created with a plugin"))
		})
	})

	Describe("newlyAddedDependents", func() {
		It("returns nothing when recheck matches the original set", func() {
			original := []*history.BackupConfig{{Timestamp: "20260101000000"}, {Timestamp: "20260101010000"}}
			recheck := []*history.BackupConfig{{Timestamp: "20260101010000"}, {Timestamp: "20260101000000"}}
			Expect(newlyAddedDependents(original, recheck)).To(BeEmpty())
		})

		It("returns the timestamp of a dependent chained on after the original resolve", func() {
			original := []*history.BackupConfig{{Timestamp: "20260101000000"}}
			recheck := []*history.BackupConfig{{Timestamp: "20260101010000"}, {Timestamp: "20260101000000"}}
			Expect(newlyAddedDependents(original, recheck)).To(Equal([]string{"20260101010000"}))
		})
	})
})
