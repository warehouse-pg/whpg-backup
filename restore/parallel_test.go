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
		index2_attach := toc.StatementWithType{ObjectType: "INDEX METADATA", ReferenceObject: "public.testindex2", Statement: `ALTER INDEX public.testindex1 ATTACH PARTITION public.testindex2;`}
		primaryKey := toc.StatementWithType{Schema: "public", Name: "table2_pkey", ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table2", Statement: `ALTER TABLE ONLY public.table2 ADD CONSTRAINT table2_pkey PRIMARY KEY (i);`}
		primaryKey_comment := toc.StatementWithType{Schema: "public", Name: "table2_pkey", ObjectType: "CONSTRAINT METADATA", ReferenceObject: "public.table2_pkey", Statement: `COMMENT ON CONSTRAINT table2_pkey ON public.table2 IS 'hello';`}
		foreignKey := toc.StatementWithType{Schema: "public", Name: "table2_fkey", ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table2", Statement: "\n\nALTER TABLE ONLY public.table2 ADD CONSTRAINT table2_fkey FOREIGN KEY (i) REFERENCES public.table1(i);\n"}
		foreignKey_comment := toc.StatementWithType{Schema: "public", Name: "table2_fkey", ObjectType: "CONSTRAINT METADATA", ReferenceObject: "public.table2_fkey", Statement: `COMMENT ON CONSTRAINT table2_fkey ON public.table2 IS 'hello';`}
		foreignKeyOnPartitionParent := toc.StatementWithType{Schema: "public", Name: "table3_fkey", ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table3", Statement: `ALTER TABLE public.table3 ADD CONSTRAINT table3_fkey FOREIGN KEY (i) REFERENCES public.table1(i);`}
		eventTrigger := toc.StatementWithType{Name: "fooeventtrigger", ObjectType: toc.OBJ_EVENT_TRIGGER, Statement: `CREATE EVENT TRIGGER fooeventtrigger ON ddl_command_start EXECUTE PROCEDURE fooproc();`}
		eventTrigger_comment := toc.StatementWithType{Name: "fooeventtrigger", ObjectType: "EVENT TRIGGER METADATA", ReferenceObject: "fooeventtrigger", Statement: `COMMENT ON EVENT TRIGGER fooeventtrigger IS 'hello';`}
		uniqueNamedLikeForeignKey := toc.StatementWithType{Schema: "public", Name: `"my FOREIGN KEY target"`, ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table1", Statement: `ALTER TABLE ONLY public.table1 ADD CONSTRAINT "my FOREIGN KEY target" UNIQUE (i);`}
		uniqueOnTableNamedLikeForeignKey := toc.StatementWithType{Schema: "public", Name: "c", ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: `public."t ADD CONSTRAINT x FOREIGN KEY (i)"`, Statement: `ALTER TABLE ONLY public."t ADD CONSTRAINT x FOREIGN KEY (i)" ADD CONSTRAINT c UNIQUE (i);`}
		checkMentioningForeignKey := toc.StatementWithType{Schema: "public", Name: "table1_check", ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: "public.table1", Statement: `ALTER TABLE public.table1 ADD CONSTRAINT table1_check CHECK (t <> ' ADD CONSTRAINT x FOREIGN KEY '::text);`}
		foreignKeyWithQuotedNames := toc.StatementWithType{Schema: `"my ""schema"`, Name: `"a ""quoted"" name"`, ObjectType: toc.OBJ_CONSTRAINT, ReferenceObject: `"my ""schema"."my.table"`, Statement: `ALTER TABLE ONLY "my ""schema"."my.table" ADD CONSTRAINT "a ""quoted"" name" FOREIGN KEY (i) REFERENCES public.table1(i);`}
		It("places all indexes in first batch when all are on different tables", func() {
			statements := []toc.StatementWithType{index1, index2, index3}
			batches := restore.BatchPostdataStatements(statements)
			Expect(batches).To(HaveLen(6))
			Expect(batches[0]).To(Equal([]toc.StatementWithType{index1, index2, index3}))
			Expect(batches[1]).To(Equal([]toc.StatementWithType{}))
		})
		It("places first index for a table in first batch, and other indexes for that table in second", func() {
			statements := []toc.StatementWithType{index1, index2, index2, index2, index3, index3}
			batches := restore.BatchPostdataStatements(statements)
			Expect(batches[0]).To(Equal([]toc.StatementWithType{index1, index2, index3}))
			Expect(batches[1]).To(Equal([]toc.StatementWithType{index2, index2, index3}))
		})
		It("places non-index objects in second batch", func() {
			statements := []toc.StatementWithType{index1, index1, trigger}
			batches := restore.BatchPostdataStatements(statements)
			Expect(batches[0]).To(Equal([]toc.StatementWithType{index1}))
			Expect(batches[1]).To(Equal([]toc.StatementWithType{index1, trigger}))
		})
		It("places postdata metadata in third batch", func() {
			statements := []toc.StatementWithType{index1, index2, index3, index2_comment, index2_tablespace, trigger, trigger_comment}
			batches := restore.BatchPostdataStatements(statements)
			Expect(batches[0]).To(Equal([]toc.StatementWithType{index1, index2, index3}))
			Expect(batches[1]).To(Equal([]toc.StatementWithType{trigger}))
			Expect(batches[2]).To(Equal([]toc.StatementWithType{index2_comment, index2_tablespace, trigger_comment}))
		})
		It("places foreign key constraints after the indexes, the other constraints and their metadata", func() {
			// The backup file lists constraints before indexes
			statements := []toc.StatementWithType{primaryKey, primaryKey_comment, foreignKey, foreignKey_comment, foreignKeyOnPartitionParent, index1, index1, index2_attach}
			batches := restore.BatchPostdataStatements(statements)
			Expect(batches[0]).To(Equal([]toc.StatementWithType{index1}))
			Expect(batches[1]).To(Equal([]toc.StatementWithType{primaryKey, index1}))
			Expect(batches[2]).To(Equal([]toc.StatementWithType{primaryKey_comment, index2_attach}))
			Expect(batches[3]).To(Equal([]toc.StatementWithType{foreignKey, foreignKeyOnPartitionParent}))
			Expect(batches[4]).To(Equal([]toc.StatementWithType{}))
			Expect(batches[5]).To(Equal([]toc.StatementWithType{foreignKey_comment}))
		})
		It("places event triggers after the foreign key constraints, and their metadata last", func() {
			statements := []toc.StatementWithType{foreignKey, index1, eventTrigger, eventTrigger_comment, trigger, trigger_comment}
			batches := restore.BatchPostdataStatements(statements)
			Expect(batches[0]).To(Equal([]toc.StatementWithType{index1}))
			Expect(batches[1]).To(Equal([]toc.StatementWithType{trigger}))
			Expect(batches[2]).To(Equal([]toc.StatementWithType{trigger_comment}))
			Expect(batches[3]).To(Equal([]toc.StatementWithType{foreignKey}))
			Expect(batches[4]).To(Equal([]toc.StatementWithType{eventTrigger}))
			Expect(batches[5]).To(Equal([]toc.StatementWithType{eventTrigger_comment}))
		})
		It("does not take a constraint for a foreign key because of its name, its table or its definition", func() {
			statements := []toc.StatementWithType{uniqueNamedLikeForeignKey, uniqueOnTableNamedLikeForeignKey, checkMentioningForeignKey, foreignKeyWithQuotedNames}
			batches := restore.BatchPostdataStatements(statements)
			Expect(batches[1]).To(Equal([]toc.StatementWithType{uniqueNamedLikeForeignKey, uniqueOnTableNamedLikeForeignKey, checkMentioningForeignKey}))
			Expect(batches[3]).To(Equal([]toc.StatementWithType{foreignKeyWithQuotedNames}))
		})
	})
})
