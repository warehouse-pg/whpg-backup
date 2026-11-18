package restore

/*
 * This file contains functions related to executing multiple SQL statements in parallel.
 */

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/warehouse-pg/common-go-libs/gplog"
)

var (
	mutex   = &sync.Mutex{}
	txMutex = &sync.Mutex{}
)

func executeStatementsForConn(statements chan toc.StatementWithType, fatalErr *error, numErrors *int32, progressBar utils.ProgressBar, whichConn int, executeInParallel bool) {
	for statement := range statements {
		if wasTerminated || *fatalErr != nil {
			if executeInParallel {
				gplog.Error("Error detected on connection %d. Terminating transactions.", whichConn)
				txMutex.Lock()
				if connectionPool.Tx[whichConn] != nil {
					connectionPool.Rollback(whichConn)
				}
				txMutex.Unlock()
			}
			return
		}

		gplog.Debug("Executing statement: %s on connection: %d", strings.TrimSpace(statement.Statement), whichConn)
		_, err := connectionPool.Exec(statement.Statement, whichConn)
		if err != nil {
			gplog.Verbose("Error encountered when executing statement: %s Error was: %s", strings.TrimSpace(statement.Statement), err.Error())
			if MustGetFlagBool(options.ON_ERROR_CONTINUE) {
				if executeInParallel {
					atomic.AddInt32(numErrors, 1)
					if statement.ObjectType == toc.OBJ_TABLE {
						mutex.Lock()
						errorTablesMetadata[statement.Schema+"."+statement.Name] = Empty{}
						mutex.Unlock()
					}
				} else {
					*numErrors = *numErrors + 1
					if statement.ObjectType == toc.OBJ_TABLE {
						errorTablesMetadata[statement.Schema+"."+statement.Name] = Empty{}
					}
				}
			} else {
				*fatalErr = err
			}
		}
		if progressBar != nil {
			progressBar.Increment()
		}
	}

	if executeInParallel {
		txMutex.Lock()
		if connectionPool.Tx[whichConn] != nil {
			connectionPool.Commit(whichConn)
		}
		txMutex.Unlock()
	}
}

func findLeastBusyConn(connStatementCounts map[int]int) int {
	minStatements := connStatementCounts[1]
	leastBusyConn := 1
	for conn, count := range connStatementCounts {
		if count < minStatements {
			leastBusyConn = conn
			minStatements = count
		}
	}
	return leastBusyConn
}

func scheduleStatementsOnWorkers(statements []toc.StatementWithType, numConns int) map[int][]toc.StatementWithType {
	splitStatements := make(map[int][]toc.StatementWithType, numConns)
	cohortConnAssignments := make(map[uint32]int)

	connStatementCounts := make(map[int]int, numConns)
	for i := 0; i < numConns; i++ {
		connStatementCounts[i] = 0
	}
	// During backup, we track objects into "cohorts" that must be backed up in the same
	// transaction to avoid lock contention and/or deadlocks. Split up statements to
	// connections by cohort.
	for i := 0; i < len(statements); i++ {
		var cohort uint32
		if len(statements[i].Tier) == 0 {
			// older backups will not have a Tier value
			cohort = 0
		} else {
			cohort = statements[i].Tier[1]
		}
		keyConn, alreadyPinned := cohortConnAssignments[cohort]
		if alreadyPinned {
			splitStatements[keyConn] = append(splitStatements[keyConn], statements[i])
			connStatementCounts[keyConn] = connStatementCounts[keyConn] + 1
		} else {
			leastBusyConn := findLeastBusyConn(connStatementCounts)
			cohortConnAssignments[cohort] = leastBusyConn
			splitStatements[leastBusyConn] = append(splitStatements[leastBusyConn], statements[i])
			connStatementCounts[leastBusyConn] = connStatementCounts[leastBusyConn] + 1
		}
	}
	return splitStatements
}

/*
 * This function creates a worker pool of N goroutines to be able to execute up
 * to N statements in parallel.
 */
func ExecutePredataStatements(statements []toc.StatementWithType, progressBar utils.ProgressBar, executeInParallel bool, whichConn ...int) int32 {
	var workerPool sync.WaitGroup
	var fatalErr error
	var numErrors int32

	if len(statements) == 0 {
		return 0
	}
	if !executeInParallel {
		tasks := make(chan toc.StatementWithType, len(statements))
		for _, statement := range statements {
			tasks <- statement
		}
		close(tasks)
		connNum := connectionPool.ValidateConnNum(whichConn...)
		executeStatementsForConn(tasks, &fatalErr, &numErrors, progressBar, connNum, executeInParallel)
	} else {
		panicChan := make(chan error)
		splitStatements := scheduleStatementsOnWorkers(statements, connectionPool.NumConns)
		chanMap := make(map[int]chan toc.StatementWithType, connectionPool.NumConns)
		// preinitialize channels to prevent concurrent read and write
		for i := 0; i < connectionPool.NumConns; i++ {
			chanMap[i] = make(chan toc.StatementWithType, len(splitStatements[i]))
			for _, statement := range splitStatements[i] {
				chanMap[i] <- statement
			}
			close(chanMap[i])
		}
		for i := 0; i < connectionPool.NumConns; i++ {
			workerPool.Add(1)
			go func(connNum int) {
				defer func() {
					if panicErr := recover(); panicErr != nil {
						panicChan <- fmt.Errorf("%v", panicErr)
					}
				}()
				defer workerPool.Done()
				connNum = connectionPool.ValidateConnNum(connNum)
				executeStatementsForConn(chanMap[connNum], &fatalErr, &numErrors, progressBar, connNum, executeInParallel)
			}(i)
		}
		workerPool.Wait()
		// Allow panics to crash from the main process, invoking DoCleanup
		select {
		case err := <-panicChan:
			gplog.Fatal(err, "")
		default:
			// no panic, nothing to do
		}
	}
	if fatalErr != nil {
		fmt.Println("")
		gplog.Fatal(fatalErr, "")
	} else if numErrors > 0 {
		fmt.Println("")
		gplog.Error("Encountered %d errors during metadata restore; see log file %s for a list of failed statements.", numErrors, gplog.GetLogFilePath())
	}

	return numErrors
}

func ExecuteStatements(statements []toc.StatementWithType, progressBar utils.ProgressBar, executeInParallel bool, whichConn ...int) int32 {
	var workerPool sync.WaitGroup
	var fatalErr error
	var numErrors int32
	tasks := make(chan toc.StatementWithType, len(statements))
	for _, statement := range statements {
		tasks <- statement
	}
	close(tasks)

	if !executeInParallel {
		connNum := connectionPool.ValidateConnNum(whichConn...)
		executeStatementsForConn(tasks, &fatalErr, &numErrors, progressBar, connNum, executeInParallel)
	} else {
		panicChan := make(chan error)
		for i := 0; i < connectionPool.NumConns; i++ {
			workerPool.Add(1)
			go func(connNum int) {
				defer func() {
					if panicErr := recover(); panicErr != nil {
						panicChan <- fmt.Errorf("%v", panicErr)
					}
				}()
				defer workerPool.Done()
				connNum = connectionPool.ValidateConnNum(connNum)
				executeStatementsForConn(tasks, &fatalErr, &numErrors, progressBar, connNum, executeInParallel)
			}(i)
		}
		workerPool.Wait()
		// Allow panics to crash from the main process, invoking DoCleanup
		select {
		case err := <-panicChan:
			gplog.Fatal(err, "")
		default:
			// no panic, nothing to do
		}
	}
	if fatalErr != nil {
		fmt.Println("")
		gplog.Fatal(fatalErr, "")
	} else if numErrors > 0 {
		fmt.Println("")
		gplog.Error("Encountered %d errors during metadata restore; see log file %s for a list of failed statements.", numErrors, gplog.GetLogFilePath())
	}

	return numErrors
}

/*
 * BatchPostdataStatements splits the postdata statements into batches that
 * are executed one after the other, in the order they are returned. The
 * statements of one batch may run in parallel.
 *
 * 1. One index of every table. There is an existing bug in Greenplum where
 *    creating indexes in parallel on an AO table that didn't have any indexes
 *    previously can cause deadlock. Once each table has at least one index,
 *    there is no more risk of deadlock.
 *
 * 2. All other postdata objects, apart from the ones named below.
 *
 * 3. Postdata metadata (e.g. ALTER INDEX, COMMENT ON). These statements
 *    cannot be run concurrently with batch two since that is where the
 *    objects they depend on are being created.
 *
 * 4. FOREIGN KEY constraints. A foreign key needs the primary key, unique
 *    constraint or unique index of the table it references. The backup file
 *    lists constraints before indexes, so a foreign key that references a
 *    plain unique index would otherwise run before that index exists. The
 *    unique index of a partitioned table only becomes usable once the
 *    indexes of its partitions are attached to it, which happens in batch
 *    three.
 *
 *    This batch must not run in parallel: adding a foreign key locks the
 *    table it is added to and then the table it references, so two tables
 *    that reference each other would deadlock. Foreign keys are not
 *    validated against the data, so adding them one by one is cheap.
 *
 * 5. Event triggers. They are created after everything else so that they do
 *    not fire on, and possibly reject, the statements of the restore itself.
 *
 * 6. The metadata of the foreign keys and of the event triggers.
 */
const (
	postdataFirstIndexes = iota
	postdataOtherObjects
	postdataMetadata
	// PostdataForeignKeyBatch is the position of the foreign key batch in the
	// result of BatchPostdataStatements
	PostdataForeignKeyBatch
	postdataEventTriggers
	postdataLateMetadata
	numPostdataBatches
)

func BatchPostdataStatements(statements []toc.StatementWithType) [][]toc.StatementWithType {
	batches := make([][]toc.StatementWithType, numPostdataBatches)
	for i := range batches {
		batches[i] = make([]toc.StatementWithType, 0)
	}

	// The comment on a constraint does not say what kind of constraint it
	// belongs to, so remember the foreign keys to recognize their comments.
	foreignKeyNames := make(map[string]bool)
	for _, statement := range statements {
		if isForeignKeyConstraint(statement) {
			foreignKeyNames[constraintKey(statement)] = true
		}
	}

	indexMap := make(map[string]bool)
	for _, statement := range statements {
		batch := postdataOtherObjects
		_, tableIndexPresent := indexMap[statement.ReferenceObject]
		if statement.ObjectType == toc.OBJ_INDEX && !tableIndexPresent {
			indexMap[statement.ReferenceObject] = true
			batch = postdataFirstIndexes
		} else if statement.ObjectType == toc.OBJ_EVENT_TRIGGER+" METADATA" ||
			(statement.ObjectType == toc.OBJ_CONSTRAINT+" METADATA" && foreignKeyNames[constraintKey(statement)]) {
			batch = postdataLateMetadata
		} else if strings.Contains(statement.ObjectType, " METADATA") {
			batch = postdataMetadata
		} else if isForeignKeyConstraint(statement) {
			batch = PostdataForeignKeyBatch
		} else if statement.ObjectType == toc.OBJ_EVENT_TRIGGER {
			batch = postdataEventTriggers
		}
		batches[batch] = append(batches[batch], statement)
	}
	return batches
}

/*
 * The table of contents does not record the type of a constraint, so it is
 * read from the statement, which gpbackup writes as
 * ALTER TABLE [ONLY] <table> ADD CONSTRAINT <name> <definition>;
 * The table and the constraint name are skipped as a whole, quoted or not, so
 * that a name which contains the words FOREIGN KEY is not taken for one.
 */
var foreignKeyConstraintRegex = regexp.MustCompile(`^\s*ALTER TABLE (?:ONLY )?(?:"(?:[^"]|"")*"|[^"\s.]+)(?:\.(?:"(?:[^"]|"")*"|[^"\s.]+))? ADD CONSTRAINT (?:"(?:[^"]|"")*"|[^"\s]+) FOREIGN KEY\b`)

func isForeignKeyConstraint(statement toc.StatementWithType) bool {
	return statement.ObjectType == toc.OBJ_CONSTRAINT && foreignKeyConstraintRegex.MatchString(statement.Statement)
}

/*
 * A constraint and its comment share the schema and the name in the table of
 * contents; the comment does not carry the table. Constraint names are only
 * unique within a table, so the comment of another constraint with the same
 * name can be taken for the comment of a foreign key. It then merely runs in
 * a later batch.
 */
func constraintKey(statement toc.StatementWithType) string {
	return utils.MakeFQN(statement.Schema, statement.Name)
}

func BatchPredataStatements(statements []toc.StatementWithType) ([]toc.StatementWithType, map[uint32][]toc.StatementWithType, []toc.StatementWithType) {
	foundNumberedTier := false
	firstTierZero := make([]toc.StatementWithType, 0)
	numberedTiers := make(map[uint32][]toc.StatementWithType)
	secondTierZero := make([]toc.StatementWithType, 0)

	for _, statement := range statements {
		// For backwards compatibility, assume restores without Tier are always 0,0.  This is also
		// used for some object types like Schemas where we haven't yet gone through the work of
		// tiering them out for parallel execution.
		if len(statement.Tier) == 0 {
			statement.Tier = []uint32{0, 0}
		}

		if statement.Tier[0] > 0 {
			foundNumberedTier = true
			numberedTiers[statement.Tier[0]] = append(numberedTiers[statement.Tier[0]], statement)
		} else if statement.Tier[0] == 0 {
			if !foundNumberedTier {
				// insert into firstTierZero
				firstTierZero = append(firstTierZero, statement)
			} else {
				// insert into secondTierZero
				secondTierZero = append(secondTierZero, statement)
			}
		}
	}
	return firstTierZero, numberedTiers, secondTierZero
}
