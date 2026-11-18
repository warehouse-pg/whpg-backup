package restore_test

import (
	"github.com/greenplum-db/gpbackup/restore"
	"github.com/greenplum-db/gpbackup/toc"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("restore/parallel tests", func() {
	Describe("BatchPostdataStatements", func() {
		index1 := toc.StatementWithType{ObjectType: toc.OBJ_INDEX, ReferenceObject: "public.table1", Statement: `CREATE INDEX testindex1 ON public.table1 USING btree(i);`}
		index2 := toc.StatementWithType{ObjectType: toc.OBJ_INDEX, ReferenceObject: "public.table2", Statement: `CREATE INDEX testindex2 ON public.table2 USING btree(i);`}
		index3 := toc.StatementWithType{ObjectType: toc.OBJ_INDEX, ReferenceObject: "public.table3", Statement: `CREATE INDEX testindex3 ON public.table3 USING btree(i);`}
		index2_comment := toc.StatementWithType{ObjectType: "INDEX METADATA", ReferenceObject: "public.testindex2", Statement: `COMMENT ON INDEX public.testindex2 IS 'hello';`}
		index2_tablespace := toc.StatementWithType{ObjectType: "INDEX METADATA", ReferenceObject: "public.testindex2", Statement: `ALTER INDEX public.testindex2 SET TABLESPACE footblspc;`}
		trigger := toc.StatementWithType{ObjectType: toc.OBJ_TRIGGER, ReferenceObject: "public.table3", Statement: `CREATE TRIGGER footrigger AFTER INSERT ON table1 FOR EACH STATEMENT EXECUTE PROCEDURE fooproc();`}
		trigger_comment := toc.StatementWithType{ObjectType: "TRIGGER METADATA", ReferenceObject: "footrigger", Statement: `COMMENT ON TRIGGER footrigger ON table1 IS 'hello'`}
		primaryKey := toc.StatementWithType{ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table2", Statement: `ALTER TABLE ONLY public.table2 ADD CONSTRAINT table2_pkey PRIMARY KEY (i);`}
		foreignKey := toc.StatementWithType{ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table2", Statement: `ALTER TABLE ONLY public.table2 ADD CONSTRAINT table2_fkey FOREIGN KEY (i) REFERENCES public.table1(i);`}
		foreignKeyOnPartitionParent := toc.StatementWithType{ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table3", Statement: `ALTER TABLE public.table3 ADD CONSTRAINT table3_fkey FOREIGN KEY (i) REFERENCES public.table1(i);`}
		foreignKey_comment := toc.StatementWithType{ObjectType: "CONSTRAINT METADATA", ReferenceObject: "public.table2", Statement: `COMMENT ON CONSTRAINT table2_fkey ON public.table2 IS 'hello';`}
		uniqueNamedLikeForeignKey := toc.StatementWithType{ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table1", Statement: `ALTER TABLE ONLY public.table1 ADD CONSTRAINT "my FOREIGN KEY target" UNIQUE (i);`}
		checkMentioningForeignKey := toc.StatementWithType{ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table1", Statement: `ALTER TABLE public.table1 ADD CONSTRAINT table1_check CHECK (t <> ' FOREIGN KEY '::text);`}
		foreignKeyWithQuotedName := toc.StatementWithType{ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table2", Statement: `ALTER TABLE ONLY public.table2 ADD CONSTRAINT "a ""quoted"" name" FOREIGN KEY (i) REFERENCES public.table1(i);`}
		It("places all indexes in first batch when all are on different tables", func() {
			statements := []toc.StatementWithType{index1, index2, index3}
			firstBatch, secondBatch, _, _ := restore.BatchPostdataStatements(statements)
			Expect(firstBatch).To(Equal([]toc.StatementWithType{index1, index2, index3}))
			Expect(secondBatch).To(Equal([]toc.StatementWithType{}))
		})
		It("places first index for a table in first batch, and other indexes for that table in second", func() {
			statements := []toc.StatementWithType{index1, index2, index2, index2, index3, index3}
			firstBatch, secondBatch, _, _ := restore.BatchPostdataStatements(statements)
			Expect(firstBatch).To(Equal([]toc.StatementWithType{index1, index2, index3}))
			Expect(secondBatch).To(Equal([]toc.StatementWithType{index2, index2, index3}))
		})
		It("places non-index objects in second batch", func() {
			statements := []toc.StatementWithType{index1, index1, trigger}
			firstBatch, secondBatch, _, _ := restore.BatchPostdataStatements(statements)
			Expect(firstBatch).To(Equal([]toc.StatementWithType{index1}))
			Expect(secondBatch).To(Equal([]toc.StatementWithType{index1, trigger}))
		})
		It("places postdata metadata in third batch", func() {
			statements := []toc.StatementWithType{index1, index2, index3, index2_comment, index2_tablespace, trigger, trigger_comment}
			firstBatch, secondBatch, _, metadataBatch := restore.BatchPostdataStatements(statements)
			Expect(firstBatch).To(Equal([]toc.StatementWithType{index1, index2, index3}))
			Expect(secondBatch).To(Equal([]toc.StatementWithType{trigger}))
			Expect(metadataBatch).To(Equal([]toc.StatementWithType{index2_comment, index2_tablespace, trigger_comment}))
		})
		It("places foreign key constraints in a batch after the indexes and the other constraints", func() {
			// The backup file lists constraints before indexes
			statements := []toc.StatementWithType{primaryKey, foreignKey, foreignKeyOnPartitionParent, index1, index1, foreignKey_comment}
			firstBatch, secondBatch, foreignKeyBatch, metadataBatch := restore.BatchPostdataStatements(statements)
			Expect(firstBatch).To(Equal([]toc.StatementWithType{index1}))
			Expect(secondBatch).To(Equal([]toc.StatementWithType{primaryKey, index1}))
			Expect(foreignKeyBatch).To(Equal([]toc.StatementWithType{foreignKey, foreignKeyOnPartitionParent}))
			Expect(metadataBatch).To(Equal([]toc.StatementWithType{foreignKey_comment}))
		})
		It("does not take a constraint for a foreign key because of its name or its definition", func() {
			statements := []toc.StatementWithType{uniqueNamedLikeForeignKey, checkMentioningForeignKey, foreignKeyWithQuotedName}
			_, secondBatch, foreignKeyBatch, _ := restore.BatchPostdataStatements(statements)
			Expect(secondBatch).To(Equal([]toc.StatementWithType{uniqueNamedLikeForeignKey, checkMentioningForeignKey}))
			Expect(foreignKeyBatch).To(Equal([]toc.StatementWithType{foreignKeyWithQuotedName}))
		})
	})
})
