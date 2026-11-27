package restore

import (
	"fmt"
	"strings"
	"sync"

	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/spf13/pflag"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/testhelper"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("restore internal tests", func() {
	statements := []toc.StatementWithType{
		{ // simple table
			Schema: "foo", Name: "bar", ObjectType: toc.OBJ_TABLE,
			Statement: "\n\nCREATE TABLE foo.bar (\n\ti integer\n) DISTRIBUTED BY (i);\n",
		},
		{ // simple schema
			Schema: "foo", Name: "foo", ObjectType: toc.OBJ_SCHEMA,
			Statement: "\n\nCREATE SCHEMA foo;\n",
		},
		{ // table with a schema containing dots
			Schema: "\"foo.bar\"", Name: "baz", ObjectType: toc.OBJ_TABLE,
			Statement: "\n\nCREATE TABLE \"foo.bar\".baz (\n\ti integer\n) DISTRIBUTED BY (i);\n",
		},
		{ // table with a schema containing quotes
			Schema: "\"foo\"bar\"", Name: "baz", ObjectType: toc.OBJ_TABLE,
			Statement: "\n\nCREATE TABLE \"foo\"bar\".baz (\n\ti integer\n) DISTRIBUTED BY (i);\n",
		},
		{ // view with multiple schema replacements
			Schema: "foo", Name: "myview", ObjectType: toc.OBJ_VIEW,
			Statement: "\n\nCREATE VIEW foo.myview AS  SELECT bar.i\n   FROM foo.bar;\n",
		},
		{ // schema and table are the same name
			Schema: "foo", Name: "foo", ObjectType: toc.OBJ_TABLE,
			Statement: "\n\nCREATE TABLE foo.foo (\n\ti integer\n) DISTRIBUTED BY (i);\n",
		},
		{ // multi-line permissions block for a schema
			Schema: "foo", Name: "foo", ObjectType: toc.OBJ_SCHEMA,
			Statement: "\n\nREVOKE ALL ON SCHEMA foo FROM PUBLIC;\nGRANT ALL ON SCHEMA foo TO testuser;\n",
		},
		{ // multi-line permissions block for a view
			Schema: "foo", Name: "myview", ObjectType: toc.OBJ_VIEW,
			Statement: "\n\nREVOKE ALL ON TABLE foo.myview FROM PUBLIC;\nGRANT ALL ON TABLE foo.myview TO testuser;\n",
		},
		{ // multi-line permissions block for a non-schema object
			Schema: "foo", Name: "myfunc", ObjectType: toc.OBJ_FUNCTION,
			Statement: "\n\nREVOKE ALL ON FUNCTION foo.myfunc(integer) FROM PUBLIC;\nGRANT ALL ON FUNCTION foo.myfunc(integer) TO testuser;\n",
		},
		{ // multi-line permissions block with a schema containing dots
			Schema: "\"foo.bar\"", Name: "myfunc", ObjectType: toc.OBJ_FUNCTION,
			Statement: "\n\nREVOKE ALL ON FUNCTION \"foo.bar\".myfunc(integer) FROM PUBLIC;\nGRANT ALL ON FUNCTION \"foo.bar\".myfunc(integer) TO testuser;\n",
		},
		{ // ALTER TABLE ... ATTACH PARTITION statement
			Schema: "public", Name: "foopart_p1", ObjectType: toc.OBJ_TABLE, ReferenceObject: "public.foopart",
			Statement: "\n\nALTER TABLE public.foopart ATTACH PARTITION public.foopart_p1 FOR VALUES FROM (0) TO (1);\n",
		},
		{ // ALTER TABLE ONLY ... ATTACH PARTITION statement
			Schema: "public", Name: "foopart_p1", ObjectType: toc.OBJ_TABLE, ReferenceObject: "public.foopart",
			Statement: "\n\nALTER TABLE ONLY public.foopart ATTACH PARTITION public.foopart_p1 FOR VALUES FROM (0) TO (1);\n",
		},
	}
	Describe("editStatementsRedirectStatements", func() {
		It("does not alter schemas if no redirect was specified", func() {
			originalStatements := make([]toc.StatementWithType, len(statements))
			copy(originalStatements, statements)

			editStatementsRedirectSchema(statements, "")

			// Loop through statements individually instead of comparing the whole arrays directly,
			// to make it easier to find the statements with issues
			for i := range statements {
				Expect(statements[i]).To(Equal(originalStatements[i]))
			}
		})
		It("changes schema in the sql statement", func() {
			// We need to temporarily set the version to 7 or later to test the ATTACH PARTITION replacement
			oldVersion := connectionPool.Version
			connectionPool.Version = dbconn.NewVersion("7.0.0")
			defer func() { connectionPool.Version = oldVersion }()

			editStatementsRedirectSchema(statements, "foo2")

			expectedStatements := []toc.StatementWithType{
				{
					Schema: "foo2", Name: "bar", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCREATE TABLE foo2.bar (\n\ti integer\n) DISTRIBUTED BY (i);\n",
				},
				{
					Schema: "foo2", Name: "foo2", ObjectType: toc.OBJ_SCHEMA,
					Statement: "\n\nCREATE SCHEMA foo2;\n",
				},
				{
					Schema: "foo2", Name: "baz", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCREATE TABLE foo2.baz (\n\ti integer\n) DISTRIBUTED BY (i);\n",
				},
				{
					Schema: "foo2", Name: "baz", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCREATE TABLE foo2.baz (\n\ti integer\n) DISTRIBUTED BY (i);\n",
				},
				{
					Schema: "foo2", Name: "myview", ObjectType: toc.OBJ_VIEW,
					Statement: "\n\nCREATE VIEW foo2.myview AS  SELECT bar.i\n   FROM foo.bar;\n",
				},
				{
					Schema: "foo2", Name: "foo", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCREATE TABLE foo2.foo (\n\ti integer\n) DISTRIBUTED BY (i);\n",
				},
				{
					Schema: "foo2", Name: "foo2", ObjectType: toc.OBJ_SCHEMA,
					Statement: "\n\nREVOKE ALL ON SCHEMA foo2 FROM PUBLIC;\nGRANT ALL ON SCHEMA foo2 TO testuser;\n",
				},
				{
					Schema: "foo2", Name: "myview", ObjectType: toc.OBJ_VIEW,
					Statement: "\n\nREVOKE ALL ON TABLE foo2.myview FROM PUBLIC;\nGRANT ALL ON TABLE foo2.myview TO testuser;\n",
				},
				{
					Schema: "foo2", Name: "myfunc", ObjectType: toc.OBJ_FUNCTION,
					Statement: "\n\nREVOKE ALL ON FUNCTION foo2.myfunc(integer) FROM PUBLIC;\nGRANT ALL ON FUNCTION foo2.myfunc(integer) TO testuser;\n",
				},
				{
					Schema: "foo2", Name: "myfunc", ObjectType: toc.OBJ_FUNCTION,
					Statement: "\n\nREVOKE ALL ON FUNCTION foo2.myfunc(integer) FROM PUBLIC;\nGRANT ALL ON FUNCTION foo2.myfunc(integer) TO testuser;\n",
				},
				{ // ALTER TABLE ... ATTACH PARTITION statement
					Schema: "foo2", Name: "foopart_p1", ObjectType: toc.OBJ_TABLE, ReferenceObject: "foo2.foopart",
					Statement: "\n\nALTER TABLE foo2.foopart ATTACH PARTITION foo2.foopart_p1 FOR VALUES FROM (0) TO (1);\n",
				},
				{ // ALTER TABLE ONLY ... ATTACH PARTITION statement
					Schema: "foo2", Name: "foopart_p1", ObjectType: toc.OBJ_TABLE, ReferenceObject: "foo2.foopart",
					Statement: "\n\nALTER TABLE ONLY foo2.foopart ATTACH PARTITION foo2.foopart_p1 FOR VALUES FROM (0) TO (1);\n",
				},
			}

			for i := range statements {
				//fmt.Println("\n\nACTUAL\n", statements[i], "\nEXPECTED\n", expectedStatements[i])
				Expect(statements[i]).To(Equal(expectedStatements[i]))
			}
		})
	})

	Describe("removeAttributeVersionOptions", func() {
		create := "\n\nCREATE TABLE public.foo (\n\ti integer,\n\tj text\n) DISTRIBUTED BY (i);\n"
		It("drops a column statement that only sets the attribute version", func() {
			stmts := []toc.StatementWithType{{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
				Statement: create +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN i SET (pg_attver=2462773192);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET STATISTICS 5;" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (pg_attver=2462773192);"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(create +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET STATISTICS 5;"))
		})
		It("keeps the other options of a column", func() {
			stmts := []toc.StatementWithType{{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
				Statement: create +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN i SET (pg_attver=7,n_distinct=1);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (n_distinct=7,pg_attver=7,n_distinct_inherited=3);\n"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(create +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN i SET (n_distinct=1);" +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (n_distinct=7,n_distinct_inherited=3);\n"))
		})
		It("handles a foreign table and a quoted name", func() {
			stmts := []toc.StatementWithType{{Schema: "public", Name: `"my tab"`, ObjectType: toc.OBJ_FOREIGN_TABLE,
				Statement: "\n\nCREATE FOREIGN TABLE public.\"my tab\" (\n\t\"a col\" integer\n) SERVER s ;\n" +
					"\nALTER TABLE ONLY public.\"my tab\" ALTER COLUMN \"a col\" SET (pg_attver=12);"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal("\n\nCREATE FOREIGN TABLE public.\"my tab\" (\n\t\"a col\" integer\n) SERVER s ;\n"))
		})
		It("handles names that contain line breaks and quotes", func() {
			createNl := "\n\nCREATE TABLE public.\"nl\ntab\" (\n\tid integer,\n\t\"c\n\"\"x\" text\n) DISTRIBUTED BY (id);\n"
			stmts := []toc.StatementWithType{{Schema: "public", Name: "\"nl\ntab\"", ObjectType: toc.OBJ_TABLE,
				Statement: createNl +
					"\nALTER TABLE ONLY public.\"nl\ntab\" ALTER COLUMN id SET (pg_attver=12);" +
					"\nALTER TABLE ONLY public.\"nl\ntab\" ALTER COLUMN \"c\n\"\"x\" SET STORAGE EXTERNAL;" +
					"\nALTER TABLE ONLY public.\"nl\ntab\" ALTER COLUMN \"c\n\"\"x\" SET (pg_attver=12,n_distinct=2);"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(createNl +
				"\nALTER TABLE ONLY public.\"nl\ntab\" ALTER COLUMN \"c\n\"\"x\" SET STORAGE EXTERNAL;" +
				"\nALTER TABLE ONLY public.\"nl\ntab\" ALTER COLUMN \"c\n\"\"x\" SET (n_distinct=2);"))
		})
		It("removes the option whatever its value is, as gpbackup does", func() {
			stmts := []toc.StatementWithType{{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
				Statement: create +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN i SET (pg_attver=-123456);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (n_distinct=1,pg_attver=abc);"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(create +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (n_distinct=1);"))
		})
		It("handles a column name that contains the start of a column statement", func() {
			name := "\"q\nALTER TABLE ONLY public.foo ALTER COLUMN z\""
			stmts := []toc.StatementWithType{{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
				Statement: create +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN i SET (pg_attver=3);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN " + name + " SET (pg_attver=3,n_distinct=2);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN " + name + " SET (pg_attver=3);"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(create +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN " + name + " SET (n_distinct=2);"))
		})
		It("handles a table with many columns", func() {
			var statement, expected strings.Builder
			statement.WriteString(create)
			expected.WriteString(create)
			for c := 0; c < 1600; c++ {
				fmt.Fprintf(&statement, "\nALTER TABLE ONLY public.foo ALTER COLUMN c%d SET (pg_attver=9);", c)
				if c%400 == 0 {
					fmt.Fprintf(&statement, "\nALTER TABLE ONLY public.foo ALTER COLUMN c%d SET STATISTICS 5;", c)
					fmt.Fprintf(&expected, "\nALTER TABLE ONLY public.foo ALTER COLUMN c%d SET STATISTICS 5;", c)
				}
			}
			stmts := []toc.StatementWithType{{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE, Statement: statement.String()}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(expected.String()))
		})
		It("reads past a column compression statement", func() {
			stmts := []toc.StatementWithType{{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
				Statement: create +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN i SET (pg_attver=7);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (pg_attver=7,n_distinct=1);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET COMPRESSION lz4;"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(create +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (n_distinct=1);" +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET COMPRESSION lz4;"))
		})
		It("keeps a subpartition template statement in front of the column statements", func() {
			template := "ALTER TABLE public.foo \nSET SUBPARTITION TEMPLATE  \n          (\n          SUBPARTITION a VALUES(1), \n          SUBPARTITION b VALUES(2)\n          );\n"
			stmts := []toc.StatementWithType{{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
				Statement: create + template +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN i SET (pg_attver=7);" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET STATISTICS 5;" +
					"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (pg_attver=7,n_distinct=1);"}}
			removeAttributeVersionOptions(stmts)
			Expect(stmts[0].Statement).To(Equal(create + template +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET STATISTICS 5;" +
				"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (n_distinct=1);"))
		})
		It("leaves statements without the option, and other object types, alone", func() {
			stmts := []toc.StatementWithType{
				{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
					Statement: create + "\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (n_distinct=1);"},
				{Schema: "public", Name: "f", ObjectType: toc.OBJ_FUNCTION,
					Statement: "\n\nCREATE FUNCTION public.f() RETURNS text AS $$\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (pg_attver=1);\n$$ LANGUAGE sql;\n"},
				{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCREATE TABLE public.foo (\n\tj text DEFAULT 'pg_attver=1'::text\n) DISTRIBUTED RANDOMLY;\n" +
						"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (xpg_attver=1,pg_attverx=2);"},
				{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCOMMENT ON COLUMN public.foo.j IS 'see\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (pg_attver=1);\n';\n"},
				{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCREATE TABLE public.foo (\n\tj text DEFAULT 'x\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (pg_attver=1);'::text\n) DISTRIBUTED RANDOMLY;\n" +
						"\nALTER TABLE ONLY public.foo ALTER COLUMN j SET STATISTICS 5;"},
				{Schema: "public", Name: "foo", ObjectType: toc.OBJ_TABLE,
					Statement: "\n\nCREATE TABLE public.foo (\n\tj text DEFAULT 'x\nALTER TABLE ONLY public.foo ALTER COLUMN j SET (pg_attver=1);\n'::text\n) DISTRIBUTED RANDOMLY;\n"},
			}
			original := make([]toc.StatementWithType, len(stmts))
			copy(original, stmts)
			removeAttributeVersionOptions(stmts)
			for i := range stmts {
				Expect(stmts[i]).To(Equal(original[i]))
			}
		})
	})
	Describe("filterExcludedExtensionStatements", func() {
		extStatements := []toc.StatementWithType{
			{Name: "pgfs", ObjectType: toc.OBJ_EXTENSION, Statement: "CREATE EXTENSION IF NOT EXISTS pgfs WITH SCHEMA public;\n"},
			{Name: "pgfs", ObjectType: toc.OBJ_EXTENSION, Statement: "COMMENT ON EXTENSION pgfs IS 'file system access';\n"},
			{Name: "pgaa", ObjectType: toc.OBJ_EXTENSION, Statement: "CREATE EXTENSION IF NOT EXISTS pgaa WITH SCHEMA public;\n"},
			{Schema: "public", Name: "bar", ObjectType: toc.OBJ_TABLE, Statement: "CREATE TABLE public.bar (i integer);\n"},
		}

		It("returns statements unchanged when no extensions are excluded", func() {
			result := filterExcludedExtensionStatements(extStatements, []string{})
			Expect(result).To(Equal(extStatements))
		})
		It("removes all statements for an excluded extension while keeping others", func() {
			result := filterExcludedExtensionStatements(extStatements, []string{"pgfs"})

			Expect(result).To(HaveLen(2))
			Expect(result[0].Name).To(Equal("pgaa"))
			Expect(result[0].ObjectType).To(Equal(toc.OBJ_EXTENSION))
			Expect(result[1].Name).To(Equal("bar"))
			Expect(result[1].ObjectType).To(Equal(toc.OBJ_TABLE))
		})
		It("can exclude multiple extensions at once", func() {
			result := filterExcludedExtensionStatements(extStatements, []string{"pgfs", "pgaa"})

			Expect(result).To(HaveLen(1))
			Expect(result[0].Name).To(Equal("bar"))
			Expect(result[0].ObjectType).To(Equal(toc.OBJ_TABLE))
		})
		It("does not remove non-extension objects that share a name with an excluded extension", func() {
			result := filterExcludedExtensionStatements(extStatements, []string{"bar"})
			Expect(result).To(Equal(extStatements))
		})
	})

	Describe("ExpandExcludedExtensionsToSchemas", func() {
		var savedOpts *options.Options
		var savedTOC *toc.TOC
		var savedBackupConfig *history.BackupConfig

		BeforeEach(func() {
			savedOpts = opts
			savedTOC = globalTOC
			savedBackupConfig = backupConfig

			globalTOC = &toc.TOC{
				PredataEntries: []toc.MetadataEntry{
					{Schema: "pgaa", Name: "config_table", ObjectType: toc.OBJ_TABLE},
					{Schema: "public", Name: "users", ObjectType: toc.OBJ_TABLE},
				},
			}
			backupConfig = &history.BackupConfig{}
			opts = &options.Options{}
		})
		AfterEach(func() {
			opts = savedOpts
			globalTOC = savedTOC
			backupConfig = savedBackupConfig
		})

		It("does nothing when no extensions are excluded", func() {
			opts.ExcludedSchemas = []string{"existing"}

			ExpandExcludedExtensionsToSchemas()

			Expect(opts.ExcludedSchemas).To(Equal([]string{"existing"}))
		})
		It("excludes a same-named schema that exists in the backup", func() {
			opts.ExcludedExtensions = []string{"pgaa"}

			ExpandExcludedExtensionsToSchemas()

			Expect(opts.ExcludedSchemas).To(ConsistOf("pgaa"))
		})
		It("does not exclude a schema for an extension with no same-named schema, and warns", func() {
			_, _, logfile := testhelper.SetupTestLogger()
			// pgfs is installed into public, so no "pgfs" schema exists in the backup
			opts.ExcludedExtensions = []string{"pgfs"}

			ExpandExcludedExtensionsToSchemas()

			Expect(opts.ExcludedSchemas).To(BeEmpty())
			testhelper.ExpectRegexp(logfile, `[WARNING]:-No schema named "pgfs" found in the backup set for excluded extension "pgfs"`)
		})
		It("handles a mix of extensions with and without same-named schemas", func() {
			opts.ExcludedExtensions = []string{"pgfs", "pgaa"}

			ExpandExcludedExtensionsToSchemas()

			Expect(opts.ExcludedSchemas).To(ConsistOf("pgaa"))
		})
		It("does not duplicate a schema the user already excluded", func() {
			opts.ExcludedExtensions = []string{"pgaa"}
			opts.ExcludedSchemas = []string{"pgaa"}

			ExpandExcludedExtensionsToSchemas()

			Expect(opts.ExcludedSchemas).To(ConsistOf("pgaa"))
		})
	})

	Describe("DoCleanup", func() {
		BeforeEach(func() {
			cmdFlags = pflag.NewFlagSet("gprestore", pflag.ExitOnError)
			SetCmdFlags(cmdFlags)

			CleanupGroup = &sync.WaitGroup{}
			CleanupGroup.Add(1)

			wasTerminated = false
		})

		It("does not panic when globalCluster is nil and backupConfig has SingleDataFile set", func() {
			globalCluster = nil
			globalFPInfo = filepath.FilePathInfo{Timestamp: "20170101010101"}
			backupConfig = &history.BackupConfig{SingleDataFile: true}
			connectionPool = nil

			DoCleanup(true)
		})

		It("does not panic when globalCluster is nil and resize-cluster is set", func() {
			globalCluster = nil
			globalFPInfo = filepath.FilePathInfo{Timestamp: "20170101010101"}
			backupConfig = &history.BackupConfig{}
			_ = cmdFlags.Set(options.RESIZE_CLUSTER, "true")
			connectionPool = nil

			DoCleanup(true)
		})

		It("does not panic when all globals are nil", func() {
			globalCluster = nil
			globalFPInfo = filepath.FilePathInfo{}
			backupConfig = nil
			connectionPool = nil

			DoCleanup(true)
		})
	})
})
