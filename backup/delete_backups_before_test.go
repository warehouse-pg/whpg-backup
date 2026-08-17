package backup

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/warehouse-pg/common-go-libs/cluster"
	"github.com/warehouse-pg/common-go-libs/testhelper"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("delete-backups-before internal tests", func() {
	var historyDBPath = "/tmp/delete_backups_before_test_history.db"
	var testCluster *cluster.Cluster

	newConfig := func(ts string, restorePlan []history.RestorePlanEntry) history.BackupConfig {
		return history.BackupConfig{
			DatabaseName:     "testdb",
			ExcludeRelations: []string{},
			ExcludeSchemas:   []string{},
			IncludeRelations: []string{},
			IncludeSchemas:   []string{},
			Incremental:      len(restorePlan) > 1,
			RestorePlan:      restorePlan,
			Timestamp:        ts,
		}
	}

	BeforeEach(func() {
		_ = os.Remove(historyDBPath)
		testCluster = cluster.NewCluster([]cluster.SegConfig{
			{DbID: 1, ContentID: -1, Role: "p", DataDir: "/data/master", Hostname: "coordinator"},
		})
		testCluster.Executor = &testhelper.TestExecutor{ClusterOutput: &cluster.RemoteOutput{}}
	})

	AfterEach(func() {
		_ = os.Remove(historyDBPath)
	})

	Describe("deleteBackupChain, looped like delete-backups-before does", func() {
		It("deletes each independent candidate in turn", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()

			full1 := newConfig("20260101000000", []history.RestorePlanEntry{{Timestamp: "20260101000000"}})
			full2 := newConfig("20260102000000", []history.RestorePlanEntry{{Timestamp: "20260102000000"}})
			Expect(history.StoreBackupHistory(db, &full1)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &full2)).To(Succeed())

			opts := deleteChainOptions{noPrompt: true, segCluster: testCluster}
			for _, ts := range []string{full1.Timestamp, full2.Timestamp} {
				deleted, err := deleteBackupChain(db, ts, opts)
				Expect(err).To(BeNil())
				Expect(deleted).To(Equal(1))
			}

			for _, ts := range []string{full1.Timestamp, full2.Timestamp} {
				bc, err := history.GetBackupConfig(ts, db)
				Expect(err).To(BeNil())
				Expect(isFullyDeleted(bc.DateDeleted)).To(BeTrue())
			}
		})

		It("deletes a dependent along with its base in one call when cascade is set, then no-ops on the dependent's own turn", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()

			full := newConfig("20260101000000", []history.RestorePlanEntry{{Timestamp: "20260101000000"}})
			incr := newConfig("20260101010000", []history.RestorePlanEntry{
				{Timestamp: "20260101000000", TableFQNs: []string{"public.foo"}},
				{Timestamp: "20260101010000", TableFQNs: []string{"public.foo"}},
			})
			Expect(history.StoreBackupHistory(db, &full)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incr)).To(Succeed())

			opts := deleteChainOptions{cascade: true, noPrompt: true, segCluster: testCluster}

			// Loop order is oldest-first, same as GetBackupTimestampsBefore.
			deleted, err := deleteBackupChain(db, full.Timestamp, opts)
			Expect(err).To(BeNil())
			Expect(deleted).To(Equal(2))

			// incr was already swept up as a dependent; its own turn in the loop is a no-op, not
			// a double-delete error.
			deleted, err = deleteBackupChain(db, incr.Timestamp, opts)
			Expect(err).To(BeNil())
			Expect(deleted).To(Equal(0))
		})

		It("returns an error (not fatal, and without suggesting --cascade) for a candidate blocked by a live dependent, leaving it for the next loop iteration to skip via a warning", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()

			full := newConfig("20260101000000", []history.RestorePlanEntry{{Timestamp: "20260101000000"}})
			incr := newConfig("20260101010000", []history.RestorePlanEntry{
				{Timestamp: "20260101000000", TableFQNs: []string{"public.foo"}},
				{Timestamp: "20260101010000", TableFQNs: []string{"public.foo"}},
			})
			Expect(history.StoreBackupHistory(db, &full)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incr)).To(Succeed())

			opts := deleteChainOptions{cascade: false, noPrompt: true, segCluster: testCluster}

			_, err := deleteBackupChain(db, full.Timestamp, opts)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("--cascade"))

			// incr has no dependents of its own, so its turn in the loop still succeeds even
			// though its base was skipped.
			deleted, err := deleteBackupChain(db, incr.Timestamp, opts)
			Expect(err).To(BeNil())
			Expect(deleted).To(Equal(1))
		})

		It("skips an incremental backup outright when skipIncremental is set, even with no dependents of its own", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()

			full := newConfig("20260101000000", []history.RestorePlanEntry{{Timestamp: "20260101000000"}})
			incr := newConfig("20260101010000", []history.RestorePlanEntry{
				{Timestamp: "20260101000000", TableFQNs: []string{"public.foo"}},
				{Timestamp: "20260101010000", TableFQNs: []string{"public.foo"}},
			})
			Expect(history.StoreBackupHistory(db, &full)).To(Succeed())
			Expect(history.StoreBackupHistory(db, &incr)).To(Succeed())

			opts := deleteChainOptions{skipIncremental: true, noPrompt: true, segCluster: testCluster}

			_, err := deleteBackupChain(db, incr.Timestamp, opts)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incremental"))

			bc, err := history.GetBackupConfig(incr.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(isFullyDeleted(bc.DateDeleted)).To(BeFalse())
		})

		It("returns an error (not fatal) for a candidate that is still actually in progress", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()

			stuck := newConfig("20260101000000", []history.RestorePlanEntry{{Timestamp: "20260101000000"}})
			stuck.Status = history.BackupStatusInProgress
			Expect(history.StoreBackupHistory(db, &stuck)).To(Succeed())

			cmd := exec.Command("sleep", "5")
			Expect(cmd.Start()).To(Succeed())
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			lockPath := fmt.Sprintf("/tmp/%s.lck", stuck.Timestamp)
			Expect(os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0644)).To(Succeed())
			defer os.Remove(lockPath)

			_, err := deleteBackupChain(db, stuck.Timestamp, deleteChainOptions{noPrompt: true})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("in progress"))
		})

		It("skips (with 0 deleted, no error) a candidate that was already deleted", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()

			full := newConfig("20260101000000", []history.RestorePlanEntry{{Timestamp: "20260101000000"}})
			Expect(history.StoreBackupHistory(db, &full)).To(Succeed())
			Expect(history.SetDateDeleted(db, full.Timestamp, "20260102000000")).To(Succeed())

			deleted, err := deleteBackupChain(db, full.Timestamp, deleteChainOptions{noPrompt: true})
			Expect(err).To(BeNil())
			Expect(deleted).To(Equal(0))
		})

		It("treats a declined confirmation prompt as 0-deleted, not an error", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()

			full := newConfig("20260101000000", []history.RestorePlanEntry{{Timestamp: "20260101000000"}})
			Expect(history.StoreBackupHistory(db, &full)).To(Succeed())

			r, w, err := os.Pipe()
			Expect(err).To(BeNil())
			_, err = w.WriteString("n\n")
			Expect(err).To(BeNil())
			Expect(w.Close()).To(Succeed())
			origStdin := os.Stdin
			os.Stdin = r
			defer func() { os.Stdin = origStdin }()

			deleted, err := deleteBackupChain(db, full.Timestamp, deleteChainOptions{noPrompt: false, stdinReader: bufio.NewReader(os.Stdin)})
			Expect(err).To(BeNil())
			Expect(deleted).To(Equal(0))
		})
	})
})
