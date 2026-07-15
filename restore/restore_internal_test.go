package restore

import (
	"sync"

	"github.com/greenplum-db/gp-common-go-libs/dbconn"
	"github.com/greenplum-db/gp-common-go-libs/testhelper"
	"github.com/greenplum-db/gpbackup/filepath"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/spf13/pflag"

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
