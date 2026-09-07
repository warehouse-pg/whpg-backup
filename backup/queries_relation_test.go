package backup_test

import (
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/greenplum-db/gpbackup/backup"
	"github.com/warehouse-pg/common-go-libs/structmatcher"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("backup internal tests", func() {
	Describe("generateLockQueries", func() {
		It("batches tables together and generates lock queries", func() {
			tables := make([]backup.Relation, 0)
			for i := 0; i < 200; i++ {
				tables = append(tables, backup.Relation{0, 0, "public", fmt.Sprintf("foo%d", i)})
			}

			batchSize := 100
			lockQueries := backup.GenerateTableBatches(tables, batchSize)
			Expect(len(lockQueries)).To(Equal(2))
		})
		It("batches up remaining leftover tables together in a single lock query", func() {
			tables := make([]backup.Relation, 0)
			for i := 0; i < 101; i++ {
				tables = append(tables, backup.Relation{0, 0, "public", fmt.Sprintf("foo%d", i)})
			}

			batchSize := 50
			lockQueries := backup.GenerateTableBatches(tables, batchSize)
			Expect(len(lockQueries)).To(Equal(3))
		})
	})
	Describe("GetAllViews", func() {
		It("GetAllViews properly handles NULL view definitions", func() {
			columnDefHeader := []string{"attrelid", "attnum", "name", "attnotnull", "atthasdef", "type", "encoding", "attstattarget", "storagetype", "defaultval", "comment", "privileges", "kind", "options", "fdwoptions", "collation", "securitylabelprovider", "securitylabel", "attgenerated", "isinherited"}
			fakeColumnDef := sqlmock.NewRows(columnDefHeader)
			mock.ExpectQuery(`SELECT (.*)`).WillReturnRows(fakeColumnDef)

			header := []string{"oid", "schema", "name", "options", "definition", "tablespace", "ismaterialized"}
			rowOne := []driver.Value{"1", "mock_schema", "mock_table", "mock_options", "mock_def", "mock_tablespace", false}
			rowTwo := []driver.Value{"2", "mock_schema2", "mock_table2", "mock_options2", nil, "mock_tablespace2", false}
			fakeRows := sqlmock.NewRows(header).AddRow(rowOne...).AddRow(rowTwo...)
			mock.ExpectQuery(`SELECT (.*)`).WillReturnRows(fakeRows)

			headerDistPol := []string{"oid", "value"}
			fakeRowsDistPol := sqlmock.NewRows(headerDistPol)
			mock.ExpectQuery(`SELECT (.*)`).WillReturnRows(fakeRowsDistPol)

			result := backup.GetAllViews(connectionPool)

			// Expect the GetAllViews function to return only the 1st row since the 2nd row has a NULL view definition
			expectedResult := []backup.View{{Oid: 1, Schema: "mock_schema", Name: "mock_table", Options: "mock_options",
				Definition: sql.NullString{String: "mock_def", Valid: true}, Tablespace: "mock_tablespace", IsMaterialized: false}}
			Expect(result).To(HaveLen(1))
			structmatcher.ExpectStructsToMatch(&expectedResult[0], &result[0])
		})
	})
	Describe("GetRelationsChangedSinceSnapshot", func() {
		tables := []backup.Relation{
			{SchemaOid: 2200, Oid: 101, Schema: "public", Name: "unchanged"},
			{SchemaOid: 2200, Oid: 102, Schema: "public", Name: "rewritten"},
			{SchemaOid: 2200, Oid: 103, Schema: "public", Name: "dropped"},
			{SchemaOid: 2200, Oid: 104, Schema: "public", Name: "parted"},
		}
		header := []string{"oid", "parentoid", "schema", "name", "snapshotrelfilenode", "currentrelfilenode"}
		// The partition root has no storage of its own; its data lives in leaf 201.
		unchangedRows := func() *sqlmock.Rows {
			return sqlmock.NewRows(header).
				AddRow(101, nil, "public", "unchanged", 1001, 1001).
				AddRow(102, nil, "public", "rewritten", 1002, 1002).
				AddRow(103, nil, "public", "dropped", 1003, 1003).
				AddRow(104, nil, "public", "parted", 0, nil).
				AddRow(201, 104, "public", "parted_1_prt_1", 2001, 2001)
		}

		It("returns nothing without querying when there are no tables", func() {
			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, []backup.Relation{})
			Expect(changed).To(BeEmpty())
			Expect(mock.ExpectationsWereMet()).To(Succeed())
		})
		It("returns nothing when every relation still has the relfilenode the snapshot saw", func() {
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(unchangedRows())

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, tables)
			Expect(changed).To(BeEmpty())
		})
		It("reports a table whose live relfilenode differs from the snapshot as rewritten", func() {
			rows := sqlmock.NewRows(header).
				AddRow(101, nil, "public", "unchanged", 1001, 1001).
				AddRow(102, nil, "public", "rewritten", 1002, 2002).
				AddRow(103, nil, "public", "dropped", 1003, 1003)
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(rows)

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, tables)
			Expect(changed).To(HaveLen(1))
			Expect(changed[0].Relation).To(Equal(tables[1]))
			Expect(changed[0].Dropped).To(BeFalse())
			Expect(changed[0].Ancestors).To(BeEmpty())
		})
		It("reports a table with no live relfilenode as dropped", func() {
			rows := sqlmock.NewRows(header).
				AddRow(101, nil, "public", "unchanged", 1001, 1001).
				AddRow(102, nil, "public", "rewritten", 1002, 1002).
				AddRow(103, nil, "public", "dropped", 1003, nil)
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(rows)

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, tables)
			Expect(changed).To(HaveLen(1))
			Expect(changed[0].Relation).To(Equal(tables[2]))
			Expect(changed[0].Dropped).To(BeTrue())
		})
		It("reports a changed partition below a locked table with the table as its ancestor", func() {
			rows := sqlmock.NewRows(header).
				AddRow(104, nil, "public", "parted", 0, nil).
				AddRow(201, 104, "public", "parted_1_prt_1", 2001, 2002)
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(rows)

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, tables)
			Expect(changed).To(HaveLen(1))
			Expect(changed[0].FQN()).To(Equal("public.parted_1_prt_1"))
			Expect(changed[0].Dropped).To(BeFalse())
			Expect(changed[0].Ancestors).To(Equal([]backup.Relation{tables[3]}))
			Expect(changed[0].Describe()).To(Equal("public.parted_1_prt_1 (rewritten or truncated, partition of public.parted)"))
		})
		It("does not compare relations without storage", func() {
			rows := sqlmock.NewRows(header).
				AddRow(104, nil, "public", "parted", 0, nil)
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(rows)

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, tables)
			Expect(changed).To(BeEmpty())
		})
		It("describes a changed table with its reason", func() {
			rewritten := backup.ChangedRelation{Relation: tables[1], Dropped: false}
			dropped := backup.ChangedRelation{Relation: tables[2], Dropped: true}
			Expect(rewritten.Reason()).To(Equal("rewritten or truncated"))
			Expect(rewritten.Describe()).To(Equal("public.rewritten (rewritten or truncated)"))
			Expect(dropped.Reason()).To(Equal("dropped and recreated"))
			Expect(dropped.Describe()).To(Equal("public.dropped (dropped and recreated)"))
		})
		It("lists locked tables in input order, each followed by its changed partitions", func() {
			rows := sqlmock.NewRows(header).
				AddRow(201, 104, "public", "parted_1_prt_1", 2001, 2002).
				AddRow(104, nil, "public", "parted", 0, nil).
				AddRow(103, nil, "public", "dropped", 1003, nil).
				AddRow(102, nil, "public", "rewritten", 1002, 2002)
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(rows)

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, tables)
			Expect(changed).To(HaveLen(3))
			Expect(changed[0].Name).To(Equal("rewritten"))
			Expect(changed[1].Name).To(Equal("dropped"))
			Expect(changed[2].Name).To(Equal("parted_1_prt_1"))
		})
		It("queries the locked tables in batches", func() {
			many := make([]backup.Relation, 0, 1001)
			for oid := uint32(1); oid <= 1001; oid++ {
				many = append(many, backup.Relation{SchemaOid: 2200, Oid: oid, Schema: "public", Name: fmt.Sprintf("t%d", oid)})
			}
			firstBatch := sqlmock.NewRows(header).AddRow(1, nil, "public", "t1", 1, 1)
			secondBatch := sqlmock.NewRows(header).AddRow(1001, nil, "public", "t1001", 1001, 9999)
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(firstBatch)
			mock.ExpectQuery(`WITH RECURSIVE (.*)`).WillReturnRows(secondBatch)

			changed := backup.GetRelationsChangedSinceSnapshot(connectionPool, many)
			Expect(changed).To(HaveLen(1))
			Expect(changed[0].Name).To(Equal("t1001"))
			Expect(mock.ExpectationsWereMet()).To(Succeed())
		})
	})
})
