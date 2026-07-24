package history_test

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/operating"
	"github.com/greenplum-db/gpbackup/structmatcher"
	"github.com/greenplum-db/gpbackup/testhelper"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
)

var (
	testLogfile *Buffer
)

func TestBackupHistory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "History Suite")
}

var _ = BeforeSuite(func() {
	_, _, testLogfile = testhelper.SetupTestLogger()
})

var _ = Describe("backup/history tests", func() {
	var testConfig1, testConfig2 history.BackupConfig
	var historyDBPath = "/tmp/history_db.db"

	BeforeEach(func() {
		testConfig1 = history.BackupConfig{
			DatabaseName:     "testdb1",
			ExcludeRelations: []string{},
			ExcludeSchemas:   []string{},
			IncludeRelations: []string{"testschema.testtable1", "testschema.testtable2"},
			IncludeSchemas:   []string{},
			RestorePlan:      []history.RestorePlanEntry{},
			Timestamp:        "timestamp1",
		}
		testConfig2 = history.BackupConfig{
			DatabaseName:     "testdb1",
			ExcludeRelations: []string{},
			ExcludeSchemas:   []string{},
			IncludeRelations: []string{"testschema.testtable1", "testschema.testtable2"},
			IncludeSchemas:   []string{},
			RestorePlan:      []history.RestorePlanEntry{{"timestamp1", []string{"testschema.testtable1"}}, {"timestamp2", []string{"testschema.testtable2"}}},
			Timestamp:        "timestamp2",
		}
		_ = os.Remove(historyDBPath)
	})

	AfterEach(func() {
		_ = os.Remove(historyDBPath)
	})
	Describe("CurrentTimestamp", func() {
		It("returns the current timestamp", func() {
			operating.System.Now = func() time.Time { return time.Date(2017, time.January, 1, 1, 1, 1, 1, time.Local) }
			expected := "20170101010101"
			actual := history.CurrentTimestamp()
			Expect(actual).To(Equal(expected))
		})
	})
	Describe("InitializeHistoryDatabase", func() {
		It("creates, initializes, and returns a handle to the database if none is already present", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			tablesRow, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' order by name;")
			Expect(err).To(BeNil())

			var tableNames []string
			for tablesRow.Next() {
				var exclSchema string
				err = tablesRow.Scan(&exclSchema)
				Expect(err).To(BeNil())
				tableNames = append(tableNames, exclSchema)
			}

			Expect(tableNames[0]).To(Equal("backups"))
			Expect(tableNames[1]).To(Equal("exclude_relations"))
			Expect(tableNames[2]).To(Equal("exclude_schemas"))
			Expect(tableNames[3]).To(Equal("include_relations"))
			Expect(tableNames[4]).To(Equal("include_schemas"))
			Expect(tableNames[5]).To(Equal("restore_plan_tables"))
			Expect(tableNames[6]).To(Equal("restore_plans"))

		})

		It("returns a handle to an existing database if one is already present", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			createDummyTable := "CREATE TABLE IF NOT EXISTS dummy (dummy int);"
			_, _ = db.Exec(createDummyTable)
			db.Close()

			sameDB, _ := history.InitializeHistoryDatabase(historyDBPath)
			tableRow := sameDB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' and name='dummy';")

			var tableName string
			err := tableRow.Scan(&tableName)
			Expect(err).To(BeNil())
			Expect(tableName).To(Equal("dummy"))

		})

		It("adds single_backup_dir, command_line, and object_count to a backups table that predates them", func() {
			// Simulate a gpbackup_history.db created before this ticket's columns existed, since
			// CREATE TABLE IF NOT EXISTS is a no-op against a pre-existing backups table and would
			// otherwise leave old databases without these columns.
			legacyDB, err := sql.Open("sqlite3", historyDBPath)
			Expect(err).To(BeNil())
			_, err = legacyDB.Exec(`
				CREATE TABLE backups (
					timestamp TEXT NOT NULL PRIMARY KEY,
					backup_dir TEXT,
					backup_version TEXT,
					compressed INT CHECK (compressed in (0,1)),
					compression_type TEXT,
					database_name TEXT,
					database_version TEXT,
					segment_count INT,
					data_only INT CHECK (data_only in (0,1)),
					date_deleted TEXT,
					exclude_schema_filtered INT CHECK (exclude_schema_filtered in (0,1)),
					exclude_table_filtered INT CHECK (exclude_table_filtered in (0,1)),
					include_schema_filtered INT CHECK (include_schema_filtered in (0,1)),
					include_table_filtered INT CHECK (include_table_filtered in (0,1)),
					incremental INT CHECK (incremental in (0,1)),
					leaf_partition_data INT CHECK (leaf_partition_data in (0,1)),
					metadata_only INT CHECK (metadata_only in (0,1)),
					plugin TEXT,
					plugin_version TEXT,
					single_data_file INT CHECK (single_data_file in (0,1)),
					end_time TEXT,
					without_globals INT CHECK (without_globals in (0,1)),
					with_statistics INT CHECK (with_statistics in (0,1)),
					status TEXT
				);`)
			Expect(err).To(BeNil())
			_, err = legacyDB.Exec("INSERT INTO backups (timestamp, backup_dir, database_name, status) VALUES ('legacyts', '/data/backups', 'legacydb', 'Success')")
			Expect(err).To(BeNil())
			Expect(legacyDB.Close()).To(BeNil())

			migratedDB, err := history.InitializeHistoryDatabase(historyDBPath)
			Expect(err).To(BeNil())
			defer migratedDB.Close()

			columnRows, err := migratedDB.Query("PRAGMA table_info(backups);")
			Expect(err).To(BeNil())
			columnNames := make(map[string]bool)
			for columnRows.Next() {
				var cid, notNull, pk int
				var name, colType string
				var dfltValue sql.NullString
				err = columnRows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk)
				Expect(err).To(BeNil())
				columnNames[name] = true
			}
			Expect(columnRows.Close()).To(BeNil())
			Expect(columnNames).To(HaveKey("single_backup_dir"))
			Expect(columnNames).To(HaveKey("command_line"))
			Expect(columnNames).To(HaveKey("object_count"))

			// The pre-existing row must survive the migration untouched.
			legacyRow := migratedDB.QueryRow("SELECT database_name, status FROM backups WHERE timestamp = 'legacyts'")
			var dbName, status string
			err = legacyRow.Scan(&dbName, &status)
			Expect(err).To(BeNil())
			Expect(dbName).To(Equal("legacydb"))
			Expect(status).To(Equal("Success"))

			// A backup stored after migration should round-trip through the newly added columns.
			migratedConfig := history.BackupConfig{
				DatabaseName:     "testdb1",
				ExcludeRelations: []string{},
				ExcludeSchemas:   []string{},
				IncludeRelations: []string{},
				IncludeSchemas:   []string{},
				RestorePlan:      []history.RestorePlanEntry{},
				Timestamp:        "migratedts",
				SingleBackupDir:  true,
				CommandLine:      "gpbackup --dbname testdb1 --single-backup-dir",
				ObjectCount:      7,
			}
			err = history.StoreBackupHistory(migratedDB, &migratedConfig)
			Expect(err).To(BeNil())

			config, err := history.GetBackupConfig("migratedts", migratedDB)
			Expect(err).To(BeNil())
			Expect(config).To(structmatcher.MatchStruct(migratedConfig))
		})
	})

	Describe("StoreBackupHistory", func() {
		It("stores a config into the database", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			err := history.StoreBackupHistory(db, &testConfig1)
			Expect(err).To(BeNil())

			tableRow := db.QueryRow("SELECT timestamp, database_name FROM backups;")

			var timestamp string
			var dbName string
			err = tableRow.Scan(&timestamp, &dbName)
			Expect(err).To(BeNil())
			Expect(timestamp).To(Equal(testConfig1.Timestamp))
			Expect(dbName).To(Equal(testConfig1.DatabaseName))

			inclRelRows, err := db.Query("SELECT timestamp, name FROM include_relations ORDER BY name")
			Expect(err).To(BeNil())
			var includeRelations []string
			for inclRelRows.Next() {
				var inclRelTS string
				var inclRel string
				err = inclRelRows.Scan(&inclRelTS, &inclRel)
				Expect(err).To(BeNil())
				Expect(inclRelTS).To(Equal(timestamp))
				includeRelations = append(includeRelations, inclRel)
			}

			Expect(includeRelations[0]).To(Equal("testschema.testtable1"))
			Expect(includeRelations[1]).To(Equal("testschema.testtable2"))
		})

		It("refuses to store a config into the database if the timestamp is already present", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			err := history.StoreBackupHistory(db, &testConfig1)
			Expect(err).To(BeNil())

			err = history.StoreBackupHistory(db, &testConfig1)
			Expect(err.Error()).To(Equal("UNIQUE constraint failed: backups.timestamp"))
		})
	})

	Describe("GetBackupConfig", func() {
		It("gets a config from the database", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			err := history.StoreBackupHistory(db, &testConfig1)
			Expect(err).To(BeNil())

			config, err := history.GetBackupConfig(testConfig1.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(config).To(structmatcher.MatchStruct(testConfig1))
		})

		It("refuses to get a config from the database if the timestamp is not present", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			err := history.StoreBackupHistory(db, &testConfig1)
			Expect(err).To(BeNil())

			_, err = history.GetBackupConfig("timestampDNE", db)
			Expect(err.Error()).To(Equal("timestamp doesn't match any existing backups"))

		})
		It("gets a config from the database with multiple restore plan entries", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			err := history.StoreBackupHistory(db, &testConfig1)
			Expect(err).To(BeNil())
			err = history.StoreBackupHistory(db, &testConfig2)
			Expect(err).To(BeNil())

			config, err := history.GetBackupConfig(testConfig2.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(config).To(structmatcher.MatchStruct(testConfig2))
		})

		It("round-trips single_backup_dir, command_line, and object_count", func() {
			testConfig1.SingleBackupDir = true
			testConfig1.CommandLine = "gpbackup --dbname testdb1 --single-backup-dir --backup-dir /backups"
			testConfig1.ObjectCount = 123

			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			err := history.StoreBackupHistory(db, &testConfig1)
			Expect(err).To(BeNil())

			config, err := history.GetBackupConfig(testConfig1.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(config).To(structmatcher.MatchStruct(testConfig1))
			Expect(config.SingleBackupDir).To(BeTrue())
			Expect(config.CommandLine).To(Equal(testConfig1.CommandLine))
			Expect(config.ObjectCount).To(Equal(123))
		})

		It("defaults single_backup_dir, command_line, and object_count to their zero values", func() {
			db, _ := history.InitializeHistoryDatabase(historyDBPath)
			defer db.Close()
			err := history.StoreBackupHistory(db, &testConfig1)
			Expect(err).To(BeNil())

			config, err := history.GetBackupConfig(testConfig1.Timestamp, db)
			Expect(err).To(BeNil())
			Expect(config.SingleBackupDir).To(BeFalse())
			Expect(config.CommandLine).To(Equal(""))
			Expect(config.ObjectCount).To(Equal(0))
		})
	})
})
