package integration

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/greenplum-db/gpbackup/dbconn"
	fp "github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/testhelper"
	"github.com/greenplum-db/gpbackup/testutils"
	"github.com/greenplum-db/gpbackup/utils"

	"golang.org/x/sys/unix"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("utils integration", func() {
	It("TerminateHangingCopySessions stops hanging COPY sessions", func() {
		tempDir, err := ioutil.TempDir("", "temp")
		Expect(err).To(Not(HaveOccurred()))
		defer os.Remove(tempDir)
		testPipe := filepath.Join(tempDir, "test_pipe")
		conn := testutils.SetupTestDbConn("testdb")
		defer conn.Close()

		fpInfo := fp.FilePathInfo{
			PID:       1,
			Timestamp: "11223344556677",
		}

		testhelper.AssertQueryRuns(conn, "SET application_name TO 'hangingApplication'")
		testhelper.AssertQueryRuns(conn, "CREATE TABLE public.foo(i int)")
		// TODO: this works without error in 6, but throws an error in 7.  Still functions, though.  Unclear why the change.
		// defer testhelper.AssertQueryRuns(conn, "DROP TABLE public.foo")
		defer connectionPool.MustExec("DROP TABLE public.foo")
		err = unix.Mkfifo(testPipe, 0700)
		Expect(err).To(Not(HaveOccurred()))
		defer os.Remove(testPipe)
		go func() {
			// Use *sql.Conn instead of *sql.DB so database/sql cannot
			// silently retry the COPY on a fresh connection if the backend
			// is torn down — that retry spawns an orphan COPY that holds
			// public.foo's lock and deadlocks the deferred DROP TABLE.
			ctx := context.Background()
			sqlConn, cErr := conn.ConnPool[0].Conn(ctx)
			if cErr != nil {
				return
			}
			defer sqlConn.Close()
			copyFileName := fpInfo.GetSegmentPipePathForCopyCommand()
			// COPY will block because there is no reader for the testPipe
			_, _ = sqlConn.ExecContext(ctx, fmt.Sprintf("COPY public.foo TO PROGRAM 'echo %s > /dev/null; cat - > %s' WITH CSV DELIMITER ','", copyFileName, testPipe))
		}()

		// Match an active COPY only. After TerminateHangingCopySessions
		// cancels the COPY the session stays alive as idle (with the COPY
		// query still visible in pg_stat_activity.query), so the filter
		// must include state = 'active' to ever reach 0.
		query := `SELECT count(*) FROM pg_stat_activity WHERE application_name = 'hangingApplication' AND state = 'active'`
		Eventually(func() string { return dbconn.MustSelectString(connectionPool, query) }, 5*time.Second, 100*time.Millisecond).Should(Equal("1"))

		utils.TerminateHangingCopySessions(fpInfo, "hangingApplication", 30*time.Second, 1*time.Second)

		Eventually(func() string { return dbconn.MustSelectString(connectionPool, query) }, 5*time.Second, 100*time.Millisecond).Should(Equal("0"))

	})
})
