package end_to_end_test

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/greenplum-db/gpbackup/testutils"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/testhelper"
	"gopkg.in/yaml.v2"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

/*
 * gpbackup exports its snapshot before it locks the tables. A table whose
 * storage is replaced in that window (ALTER TABLE ... SET WITH (REORGANIZE=true),
 * SET DISTRIBUTED BY, TRUNCATE, VACUUM FULL, or DROP + CREATE under the same
 * name) can no longer be read consistently under that snapshot: its old files
 * are gone, so COPY sees no rows, and for AO tables the pg_aoseg helper named
 * under the snapshot no longer exists.
 *
 * These tests drive that window deterministically: a second session holds the
 * rewrite open, gpbackup blocks in LockTables behind it, the session commits,
 * and gpbackup must notice the change after the lock is granted.
 */

const (
	changedTableWarning = "changed on disk after the backup snapshot was taken"
	newSnapshotWarning  = "taking a new snapshot"
	dataSkippedWarning  = "not backed up"
)

// holdRewrite opens a transaction on a fresh connection, runs the given
// statements, and leaves the transaction open so its locks stay held.
func holdRewrite(statements string) *dbconn.DBConn {
	conn := testutils.SetupTestDbConn("testdb")
	conn.MustBegin()
	conn.MustExec(statements)
	return conn
}

// releaseRewrite rolls back a rewrite that is still open (a failed assertion
// may have skipped the commit) so a blocked gpbackup can finish, then closes
// the connection.
func releaseRewrite(conn *dbconn.DBConn) {
	if conn.Tx[0] != nil {
		_ = conn.Rollback()
	}
	conn.Close()
}

// startBackup runs gpbackup in the background with the given extra flags and
// returns a channel that is closed when it exits, plus pointers to its output
// and error.
func startBackup(extraArgs ...string) (<-chan struct{}, *string, *error) {
	done := make(chan struct{})
	var output string
	var err error
	go func() {
		defer GinkgoRecover()
		output, err = runBackup(extraArgs...)
		close(done)
	}()
	return done, &output, &err
}

// waitForBackupExit blocks until the background gpbackup exits, so a failed
// test never leaves a gpbackup process behind holding locks on testdb.
func waitForBackupExit(done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		Fail("gpbackup did not exit within 5 minutes")
	}
}

// waitForBackupLockWait waits until gpbackup's LOCK TABLE is blocked behind
// the transaction holding a rewrite. gpbackup tags its sessions with
// application_name gpbackup_<timestamp>, and a blocked LOCK TABLE shows up as
// an ungranted AccessShareLock request of one of those sessions.
func waitForBackupLockWait(conn *dbconn.DBConn) {
	query := `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON l.pid = a.pid
		WHERE a.application_name LIKE 'gpbackup_%' AND l.granted = 'f' AND l.mode = 'AccessShareLock'`
	var waiters int
	for i := 0; i < 300; i++ {
		_ = conn.Get(&waiters, query)
		if waiters > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	Fail("gpbackup did not block in LockTables within 30s")
}

// waitForLockWaiter waits until some session is queued for a lock of the
// given mode on the table, e.g. a rewrite queued behind the current holder.
func waitForLockWaiter(conn *dbconn.DBConn, schema string, table string, mode string) {
	query := fmt.Sprintf(`SELECT count(*) FROM pg_locks l, pg_class c, pg_namespace n
		WHERE l.relation = c.oid AND n.oid = c.relnamespace
		AND n.nspname = '%s' AND c.relname = '%s' AND l.granted = 'f' AND l.mode = '%s'`,
		schema, table, mode)
	var waiters int
	for i := 0; i < 300; i++ {
		_ = conn.Get(&waiters, query)
		if waiters > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	Fail(fmt.Sprintf("no session queued for %s on %s.%s within 30s", mode, schema, table))
}

// runBackup runs gpbackup with the given extra flags and returns its combined
// output and exit error.
func runBackup(extraArgs ...string) (string, error) {
	args := append([]string{"--verbose", "--dbname", "testdb", "--backup-dir", backupDir}, extraArgs...)
	cmd := exec.Command(gpbackupPath, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// runLeafPartitionBackup runs gpbackup with --leaf-partition-data, so the AO
// modcount queries run.
func runLeafPartitionBackup() (string, error) {
	return runBackup("--leaf-partition-data")
}

func readTOC(timestamp string) *toc.TOC {
	tocStruct := &toc.TOC{}
	err := yaml.Unmarshal(getMetdataFileContents(backupDir, timestamp, "toc.yaml"), tocStruct)
	Expect(err).ToNot(HaveOccurred())
	return tocStruct
}

func findDataEntry(tocStruct *toc.TOC, schema string, name string) *toc.CoordinatorDataEntry {
	for i := range tocStruct.DataEntries {
		entry := &tocStruct.DataEntries[i]
		if entry.Schema == schema && entry.Name == name {
			return entry
		}
	}
	return nil
}

func hasTableDDL(tocStruct *toc.TOC, schema string, name string) bool {
	for _, entry := range tocStruct.PredataEntries {
		if entry.ObjectType == toc.OBJ_TABLE && entry.Schema == schema && entry.Name == name {
			return true
		}
	}
	return false
}

var _ = Describe("Tables changed between snapshot and lock", func() {
	BeforeEach(func() {
		if useOldBackupVersion {
			Skip("This test is not needed for old backup versions")
		}
		end_to_end_setup()
	})
	AfterEach(func() {
		end_to_end_teardown()
	})

	It("backs up normally when no concurrent DDL happens", func() {
		output, err := runLeafPartitionBackup()
		Expect(err).ToNot(HaveOccurred(), output)
		Expect(output).To(ContainSubstring("Backup completed successfully"))
		Expect(output).ToNot(ContainSubstring(changedTableWarning))
		Expect(output).ToNot(ContainSubstring("[CRITICAL]"))

		tocStruct := readTOC(getBackupTimestamp(output))
		Expect(findDataEntry(tocStruct, "schema2", "ao1").RowsCopied).To(Equal(int64(1000)))
		Expect(tocStruct.IncrementalMetadata.AO).To(HaveKey("schema2.ao1"))
	})

	It("retries with a new snapshot when AO and heap tables are rewritten in the window", func() {
		// AO table: the rewrite replaces its pg_aoseg helper, which is what the
		// customer hit. Heap table: the rewrite replaces its relfilenode, which
		// today produces an empty table in the backup with no error at all.
		rewriter := holdRewrite(`ALTER TABLE schema2.ao1 SET WITH (reorganize=true);
			ALTER TABLE public.foo SET WITH (reorganize=true)`)
		done, output, err := startBackup("--leaf-partition-data")
		defer waitForBackupExit(done)
		defer releaseRewrite(rewriter)

		waitForBackupLockWait(backupConn)
		rewriter.MustCommit()
		waitForBackupExit(done)

		Expect(*err).ToNot(HaveOccurred(), *output)
		Expect(*output).To(ContainSubstring("Backup completed successfully"))
		Expect(*output).ToNot(ContainSubstring("[CRITICAL]"))
		Expect(*output).To(ContainSubstring(changedTableWarning))
		Expect(*output).To(ContainSubstring("schema2.ao1"))
		Expect(*output).To(ContainSubstring("public.foo"))
		Expect(*output).To(ContainSubstring(newSnapshotWarning))
		Expect(*output).ToNot(ContainSubstring(dataSkippedWarning))

		// The second attempt saw the rewritten storage, so both tables carry
		// their full data and the AO table keeps its incremental entry.
		timestamp := getBackupTimestamp(*output)
		tocStruct := readTOC(timestamp)
		Expect(findDataEntry(tocStruct, "schema2", "ao1").RowsCopied).To(Equal(int64(1000)))
		Expect(findDataEntry(tocStruct, "public", "foo").RowsCopied).To(Equal(int64(40000)))
		Expect(tocStruct.IncrementalMetadata.AO).To(HaveKey("schema2.ao1"))

		// The metadata written after the retry must restore like any other.
		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb", "--backup-dir", backupDir)
		assertDataRestored(restoreConn, map[string]int{
			"schema2.ao1": 1000,
			"public.foo":  40000,
		})
	})

	It("retries when a table is truncated and reloaded in the window", func() {
		// Without detection the COPY under the stale snapshot would return no
		// rows, silently backing up an empty table.
		rewriter := holdRewrite(`TRUNCATE schema2.ao2; INSERT INTO schema2.ao2 SELECT generate_series(1, 5)`)
		defer testhelper.AssertQueryRuns(backupConn,
			"TRUNCATE schema2.ao2; INSERT INTO schema2.ao2 SELECT generate_series(1, 1000)")
		done, output, err := startBackup("--leaf-partition-data")
		defer waitForBackupExit(done)
		defer releaseRewrite(rewriter)

		waitForBackupLockWait(backupConn)
		rewriter.MustCommit()
		waitForBackupExit(done)

		Expect(*err).ToNot(HaveOccurred(), *output)
		Expect(*output).To(ContainSubstring("Backup completed successfully"))
		Expect(*output).To(ContainSubstring(changedTableWarning))
		Expect(*output).To(ContainSubstring("schema2.ao2"))

		timestamp := getBackupTimestamp(*output)
		tocStruct := readTOC(timestamp)
		Expect(findDataEntry(tocStruct, "schema2", "ao2").RowsCopied).To(Equal(int64(5)))

		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb", "--backup-dir", backupDir)
		assertDataRestored(restoreConn, map[string]int{"schema2.ao2": 5})
	})

	It("retries when a table is dropped and recreated under the same name in the window", func() {
		// LOCK TABLE by name lands on the new table while the snapshot still
		// enumerates the old OID. The retry re-resolves the include list.
		rewriter := holdRewrite(`DROP TABLE schema2.ao1;
			CREATE TABLE schema2.ao1 (i integer) WITH (appendonly=true);
			INSERT INTO schema2.ao1 SELECT generate_series(1, 7)`)
		defer testhelper.AssertQueryRuns(backupConn,
			"TRUNCATE schema2.ao1; INSERT INTO schema2.ao1 SELECT generate_series(1, 1000)")
		done, output, err := startBackup("--leaf-partition-data")
		defer waitForBackupExit(done)
		defer releaseRewrite(rewriter)

		waitForBackupLockWait(backupConn)
		rewriter.MustCommit()
		waitForBackupExit(done)

		Expect(*err).ToNot(HaveOccurred(), *output)
		Expect(*output).To(ContainSubstring("Backup completed successfully"))
		Expect(*output).To(ContainSubstring(changedTableWarning))
		Expect(*output).To(ContainSubstring("schema2.ao1"))

		timestamp := getBackupTimestamp(*output)
		tocStruct := readTOC(timestamp)
		Expect(findDataEntry(tocStruct, "schema2", "ao1").RowsCopied).To(Equal(int64(7)))
		Expect(tocStruct.IncrementalMetadata.AO).To(HaveKey("schema2.ao1"))

		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb", "--backup-dir", backupDir)
		assertDataRestored(restoreConn, map[string]int{"schema2.ao1": 7})
	})

	It("retries when a partition is truncated while the backup copies the parent", func() {
		// Without --leaf-partition-data the parent is locked and copied and the
		// partitions are only reached through it. Truncate and reload one
		// partition so its storage changes while the row count stays the same.
		truncate := "TRUNCATE schema2.returns_1_prt_jan17"
		if backupConn.Version.Before("7") {
			truncate = "ALTER TABLE schema2.returns TRUNCATE PARTITION jan17"
		}
		rewriter := holdRewrite(fmt.Sprintf(`CREATE TEMP TABLE saved_jan17 AS SELECT * FROM schema2.returns_1_prt_jan17;
			%s; INSERT INTO schema2.returns SELECT * FROM saved_jan17`, truncate))
		done, output, err := startBackup()
		defer waitForBackupExit(done)
		defer releaseRewrite(rewriter)

		waitForBackupLockWait(backupConn)
		rewriter.MustCommit()
		waitForBackupExit(done)

		Expect(*err).ToNot(HaveOccurred(), *output)
		Expect(*output).To(ContainSubstring("Backup completed successfully"))
		Expect(*output).ToNot(ContainSubstring("[CRITICAL]"))
		Expect(*output).To(ContainSubstring(changedTableWarning))
		Expect(*output).To(ContainSubstring("schema2.returns_1_prt_jan17"))
		Expect(*output).ToNot(ContainSubstring(dataSkippedWarning))

		timestamp := getBackupTimestamp(*output)
		tocStruct := readTOC(timestamp)
		Expect(findDataEntry(tocStruct, "schema2", "returns").RowsCopied).To(Equal(int64(6)))

		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb", "--backup-dir", backupDir)
		assertDataRestored(restoreConn, map[string]int{"schema2.returns": 6})
	})

	It("skips the data of a table that keeps changing after every attempt, keeps its DDL, and completes", func() {
		// Each attempt must find the table rewritten again. Queue the next
		// rewrite behind the current one before releasing it: gpbackup's
		// AccessShareLock request is ahead of it in the lock queue, so the
		// attempt is granted, fails its check, rolls back, and only then does
		// the queued rewrite start and block the next attempt.
		const attempts = 3
		rewriter := holdRewrite("ALTER TABLE schema2.ao1 SET WITH (reorganize=true)")
		done, output, err := startBackup("--leaf-partition-data")
		defer waitForBackupExit(done)
		defer releaseRewrite(rewriter)

		for attempt := 1; attempt <= attempts; attempt++ {
			waitForBackupLockWait(backupConn)
			if attempt == attempts {
				rewriter.MustCommit()
				break
			}
			next := testutils.SetupTestDbConn("testdb")
			defer releaseRewrite(next)
			queued := make(chan struct{})
			go func(conn *dbconn.DBConn) {
				defer GinkgoRecover()
				conn.MustBegin()
				// LOCK first so the rewrite is queued as a single lock request;
				// the ALTER then runs once the lock is granted.
				conn.MustExec(`LOCK TABLE schema2.ao1 IN ACCESS EXCLUSIVE MODE;
					ALTER TABLE schema2.ao1 SET WITH (reorganize=true)`)
				close(queued)
			}(next)
			waitForLockWaiter(backupConn, "schema2", "ao1", "AccessExclusiveLock")
			rewriter.MustCommit()
			<-queued
			rewriter = next
		}
		waitForBackupExit(done)

		Expect(*err).ToNot(HaveOccurred(), *output)
		Expect(*output).To(ContainSubstring("Backup completed successfully"))
		Expect(*output).ToNot(ContainSubstring("[CRITICAL]"))
		Expect(*output).To(ContainSubstring(dataSkippedWarning))
		Expect(*output).To(ContainSubstring("schema2.ao1"))

		timestamp := getBackupTimestamp(*output)
		tocStruct := readTOC(timestamp)
		// DDL kept, data and incremental entry dropped, other tables untouched.
		Expect(hasTableDDL(tocStruct, "schema2", "ao1")).To(BeTrue())
		Expect(findDataEntry(tocStruct, "schema2", "ao1")).To(BeNil())
		Expect(tocStruct.IncrementalMetadata.AO).ToNot(HaveKey("schema2.ao1"))
		Expect(findDataEntry(tocStruct, "schema2", "ao2").RowsCopied).To(Equal(int64(1000)))
		Expect(tocStruct.IncrementalMetadata.AO).To(HaveKey("schema2.ao2"))

		reportContents := string(getMetdataFileContents(backupDir, timestamp, "report"))
		Expect(reportContents).To(ContainSubstring("schema2.ao1"))

		// The backup restores: the skipped table exists and is empty.
		gprestore(gprestorePath, restoreHelperPath, timestamp,
			"--redirect-db", "restoredb", "--backup-dir", backupDir)
		assertDataRestored(restoreConn, map[string]int{
			"schema2.ao1": 0,
			"schema2.ao2": 1000,
			"public.foo":  40000,
		})
	})
})
