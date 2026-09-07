package backup

/*
 * This file contains structs and functions related to executing specific
 * queries to gather metadata for the objects handled in predata_relations.go.
 */

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/pkg/errors"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/gplog"
)

func relationAndSchemaFilterClause() string {
	if filterRelationClause != "" {
		return filterRelationClause
	}
	filterRelationClause = SchemaFilterClause("n")
	if len(MustGetFlagStringArray(options.EXCLUDE_RELATION)) > 0 {
		excludeOids := GetOidsFromRelationList(ExcludedRelationFqns)
		if len(excludeOids) > 0 {
			filterRelationClause += fmt.Sprintf("\nAND c.oid NOT IN (%s)", strings.Join(excludeOids, ","))
		}
	}
	if len(MustGetFlagStringArray(options.INCLUDE_RELATION)) > 0 {
		includeOids := GetOidsFromRelationList(IncludedRelationFqns)
		filterRelationClause += fmt.Sprintf("\nAND c.oid IN (%s)", strings.Join(includeOids, ","))
	}
	return filterRelationClause
}

func GetOidsFromRelationList(relationFqns []options.Relation) []string {
	oidString := make([]string, len(relationFqns))
	for idx, rel := range relationFqns {
		oidString[idx] = strconv.FormatUint(uint64(rel.Oid), 10)
	}
	return oidString
}

func GetIncludedUserTableRelations(connectionPool *dbconn.DBConn, includedRelationsQuoted []options.Relation) []options.Relation {
	if len(MustGetFlagStringArray(options.INCLUDE_RELATION)) > 0 {
		return getUserTableRelationsWithIncludeFiltering(connectionPool, includedRelationsQuoted)
	}
	return getUserTableRelations(connectionPool)
}

type Relation struct {
	SchemaOid uint32
	Oid       uint32
	Schema    string
	Name      string
}

func (r Relation) FQN() string {
	return utils.MakeFQN(r.Schema, r.Name)
}

func (r Relation) GetUniqueID() UniqueID {
	return UniqueID{ClassID: PG_CLASS_OID, Oid: r.Oid}
}

/*
 * Due to the structure of our codebase, we have two identical versions of the Relation struct.
 * Convert explicitly to keep the compiler happy.
 *
 * TODO -- find a way to remove this and just have one version of this struct.
 */
func ConvertRelationsOptionsToBackup(inRelations []options.Relation) []Relation {
	outRelations := make([]Relation, len(inRelations))

	for idx, inRel := range inRelations {
		outRel := Relation{
			SchemaOid: inRel.SchemaOid,
			Oid:       inRel.Oid,
			Schema:    inRel.Schema,
			Name:      inRel.Name,
		}
		outRelations[idx] = outRel
	}
	return outRelations
}

/*
 * This function also handles exclude table filtering since the way we do
 * it is currently much simpler than the include case.
 */
func getUserTableRelations(connectionPool *dbconn.DBConn) []options.Relation {
	childPartitionFilter := ""
	if !MustGetFlagBool(options.LEAF_PARTITION_DATA) && connectionPool.Version.Before("7") {
		// Filter out non-external child partitions in GPDB6 and earlier.
		// In GPDB7+ we do not want to exclude child partitions, they function as separate tables.
		childPartitionFilter = `
	AND c.oid NOT IN (
		SELECT p.parchildrelid
		FROM pg_partition_rule p
			LEFT JOIN pg_exttable e ON p.parchildrelid = e.reloid
		WHERE e.reloid IS NULL)`
	}

	// In GPDB 7+, root partitions are marked as relkind 'p'.
	relkindFilter := `'r'`
	if connectionPool.Version.AtLeast("7") {
		relkindFilter = `'r', 'p'`
	}

	query := fmt.Sprintf(`
	SELECT n.oid AS schemaoid,
		c.oid AS oid,
		quote_ident(n.nspname) AS schema,
		quote_ident(c.relname) AS name
	FROM pg_class c
		JOIN pg_namespace n ON c.relnamespace = n.oid
	WHERE %s
		%s
		AND relkind IN (%s)
		AND %s
		ORDER BY c.oid`,
		relationAndSchemaFilterClause(), childPartitionFilter, relkindFilter, ExtensionFilterClause("c"))

	results := make([]options.Relation, 0)
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	return results
}

func getUserTableRelationsWithIncludeFiltering(connectionPool *dbconn.DBConn, includeRelationFqns []options.Relation) []options.Relation {
	// In GPDB 7+, root partitions are marked as relkind 'p'.
	relkindFilter := `'r'`
	if connectionPool.Version.AtLeast("7") {
		relkindFilter = `'r', 'p'`
	}

	includeOids := GetOidsFromRelationList(includeRelationFqns)
	oidStr := strings.Join(includeOids, ", ")
	query := fmt.Sprintf(`
	SELECT n.oid AS schemaoid,
		c.oid AS oid,
		quote_ident(n.nspname) AS schema,
		quote_ident(c.relname) AS name
	FROM pg_class c
		JOIN pg_namespace n ON c.relnamespace = n.oid
	WHERE c.oid IN (%s)
		AND relkind IN (%s)
	ORDER BY c.oid`, oidStr, relkindFilter)

	results := make([]options.Relation, 0)
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	return results
}

func GetForeignTableRelations(connectionPool *dbconn.DBConn) []Relation {
	query := fmt.Sprintf(`
	SELECT n.oid AS schemaoid,
		c.oid AS oid,
		quote_ident(n.nspname) AS schema,
		quote_ident(c.relname) AS name
	FROM pg_class c
		JOIN pg_namespace n ON c.relnamespace = n.oid
	WHERE %s
		AND relkind = 'f'
		AND %s
	ORDER BY c.oid`,
		relationAndSchemaFilterClause(), ExtensionFilterClause("c"))

	results := make([]Relation, 0)
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	return results
}

type Sequence struct {
	Relation
	OwningTableOid          string
	OwningTableSchema       string
	OwningTable             string
	OwningColumn            string
	UnqualifiedOwningColumn string
	OwningColumnAttIdentity string
	IsIdentity              bool
	Definition              SequenceDefinition
}

func (s Sequence) GetMetadataEntry() (string, toc.MetadataEntry) {
	return "predata",
		toc.MetadataEntry{
			Schema:          s.Schema,
			Name:            s.Name,
			ObjectType:      toc.OBJ_SEQUENCE,
			ReferenceObject: "",
			StartByte:       0,
			EndByte:         0,
		}
}

type SequenceDefinition struct {
	LastVal     int64
	Type        string
	StartVal    int64
	Increment   int64
	MaxVal      int64
	MinVal      int64
	CacheVal    int64
	IsCycled    bool
	IsCalled    bool
	OwningTable string
}

func GetAllSequences(connectionPool *dbconn.DBConn) []Sequence {
	atLeast7Query := fmt.Sprintf(`
		SELECT n.oid AS schemaoid,
			c.oid AS oid,
			quote_ident(n.nspname) AS schema,
			quote_ident(c.relname) AS name,
			coalesce(d.refobjid::text, '') AS owningtableoid,
			coalesce(quote_ident(m.nspname), '') AS owningtableschema,
			coalesce(quote_ident(t.relname), '') AS owningtable,
			coalesce(quote_ident(a.attname), '') AS owningcolumn,
			coalesce(a.attidentity, '') AS owningcolumnattidentity,
			coalesce(quote_ident(a.attname), '') AS unqualifiedowningcolumn,
			CASE
				WHEN d.deptype IS NULL THEN false
				ELSE d.deptype = 'i'
			END AS isidentity
		FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_depend d ON c.oid = d.objid AND d.deptype in ('a', 'i')
			LEFT JOIN pg_class t ON t.oid = d.refobjid
			LEFT JOIN pg_namespace m ON m.oid = t.relnamespace
			LEFT JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE c.relkind = 'S'
			AND %s
			AND %s
		ORDER BY n.nspname, c.relname`,
		relationAndSchemaFilterClause(), ExtensionFilterClause("c"))

	before7Query := fmt.Sprintf(`
		SELECT n.oid AS schemaoid,
			c.oid AS oid,
			quote_ident(n.nspname) AS schema,
			quote_ident(c.relname) AS name,
			coalesce(d.refobjid::text, '') AS owningtableoid,
			coalesce(quote_ident(m.nspname), '') AS owningtableschema,
			coalesce(quote_ident(t.relname), '') AS owningtable,
			coalesce(quote_ident(a.attname), '') AS owningcolumn
		FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_depend d ON c.oid = d.objid AND d.deptype = 'a'
			LEFT JOIN pg_class t ON t.oid = d.refobjid
			LEFT JOIN pg_namespace m ON m.oid = t.relnamespace
			LEFT JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE c.relkind = 'S'
			AND %s
			AND %s
		ORDER BY n.nspname, c.relname`,
		relationAndSchemaFilterClause(), ExtensionFilterClause("c"))

	query := ""
	if connectionPool.Version.Before("7") {
		query = before7Query
	} else {
		query = atLeast7Query
	}

	results := make([]Sequence, 0)
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	// Exclude owning table and owning column info for sequences
	// where owning table is excluded from backup
	excludeOids := make([]string, 0)
	if len(MustGetFlagStringArray(options.EXCLUDE_RELATION)) > 0 {
		excludeOids = GetOidsFromRelationList(ExcludedRelationFqns)
	}
	for i := range results {
		found := utils.Exists(excludeOids, results[i].OwningTableOid)
		if results[i].OwningTable != "" {
			results[i].OwningTable = fmt.Sprintf("%s.%s",
				results[i].OwningTableSchema, results[i].OwningTable)
		}
		if results[i].OwningColumn != "" {
			results[i].OwningColumn = fmt.Sprintf("%s.%s",
				results[i].OwningTable, results[i].OwningColumn)
		}
		if found {
			results[i].OwningTable = ""
			results[i].OwningColumn = ""
		}
		results[i].Definition = GetSequenceDefinition(connectionPool, results[i].FQN())
	}
	return results
}

func GetSequenceDefinition(connectionPool *dbconn.DBConn, seqName string) SequenceDefinition {
	startValQuery := ""
	if connectionPool.Version.AtLeast("6") {
		startValQuery = "start_value AS startval,"
	}

	before7Query := fmt.Sprintf(`
		SELECT last_value AS lastval,
			%s
			increment_by AS increment,
			max_value AS maxval,
			min_value AS minval,
			cache_value AS cacheval,
			is_cycled AS iscycled,
			is_called AS iscalled
		FROM %s`, startValQuery, seqName)

	atLeast7Query := fmt.Sprintf(`
		SELECT s.seqstart AS startval,
			r.last_value AS lastval,
			pg_catalog.format_type(s.seqtypid, NULL) AS type,
			s.seqincrement AS increment,
			s.seqmax AS maxval,
			s.seqmin AS minval,
			s.seqcache AS cacheval,
			s.seqcycle AS iscycled,
			r.is_called AS iscalled
		FROM %s r
		JOIN pg_sequence s ON s.seqrelid = '%s'::regclass::oid;`, seqName, seqName)

	query := ""
	if connectionPool.Version.Before("7") {
		query = before7Query
	} else {
		query = atLeast7Query
	}

	result := SequenceDefinition{}
	err := connectionPool.Get(&result, query)
	gplog.FatalOnError(err)
	return result
}

type View struct {
	Oid            uint32
	Schema         string
	Name           string
	Options        string
	Definition     sql.NullString
	Tablespace     string
	IsMaterialized bool
	DistPolicy     DistPolicy
	NeedsDummy     bool
	ColumnDefs     []ColumnDefinition
	AccessMethod   string
}

func (v View) GetMetadataEntry() (string, toc.MetadataEntry) {
	return "predata",
		toc.MetadataEntry{
			Schema:          v.Schema,
			Name:            v.Name,
			ObjectType:      v.ObjectType(),
			ReferenceObject: "",
			StartByte:       0,
			EndByte:         0,
		}
}

func (v View) GetUniqueID() UniqueID {
	return UniqueID{ClassID: PG_CLASS_OID, Oid: v.Oid}
}

func (v View) FQN() string {
	return utils.MakeFQN(v.Schema, v.Name)
}

func (v View) ObjectType() string {
	if v.IsMaterialized {
		return toc.OBJ_MATERIALIZED_VIEW
	}
	return toc.OBJ_VIEW
}

// This function retrieves both regular views and materialized views.
func GetAllViews(connectionPool *dbconn.DBConn) []View {
	columnDefs := GetColumnDefinitions(connectionPool)

	// When querying the view definition using pg_get_viewdef(), the pg function
	// obtains dependency locks that are not released until the transaction is
	// committed at the end of gpbackup session. This blocks other sessions
	// from commands that need AccessExclusiveLock (e.g. TRUNCATE).
	// NB: SAVEPOINT should be created only if there is transaction in progress
	if connectionPool.Tx[0] != nil {
		connectionPool.MustExec("SAVEPOINT gpbackup_get_views")
		defer connectionPool.MustExec("ROLLBACK TO SAVEPOINT gpbackup_get_views")
	}

	before6Query := fmt.Sprintf(`
	SELECT
		c.oid AS oid,
		quote_ident(n.nspname) AS schema,
		quote_ident(c.relname) AS name,
		pg_get_viewdef(c.oid) AS definition
	FROM pg_class c
		LEFT JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
	WHERE c.relkind IN ('m', 'v')
		AND %s
		AND %s`, relationAndSchemaFilterClause(), ExtensionFilterClause("c"))

	// Materialized views were introduced in GPDB 7 and backported to GPDB 6.2.
	// Reloptions and tablespace added to pg_class in GPDB 6
	atLeast6Query := fmt.Sprintf(`
	SELECT
		c.oid AS oid,
		quote_ident(n.nspname) AS schema,
		quote_ident(c.relname) AS name,
		pg_get_viewdef(c.oid) AS definition,
		coalesce(' WITH (' || array_to_string(c.reloptions, ', ') || ')', '') AS options,
		coalesce(quote_ident(t.spcname), '') AS tablespace,
		c.relkind='m' AS ismaterialized,
		coalesce(a.amname, '') AS accessmethod
	FROM pg_class c
		LEFT JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
		LEFT JOIN pg_am a ON c.relam = a.oid
	WHERE c.relkind IN ('m', 'v')
		AND %s
		AND %s`, relationAndSchemaFilterClause(), ExtensionFilterClause("c"))

	query := ""
	if connectionPool.Version.Before("6") {
		query = before6Query
	} else {
		query = atLeast6Query
	}

	results := make([]View, 0)
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	distPolicies := GetDistributionPolicies(connectionPool, results)

	// Remove all views that have NULL definitions. This can happen
	// if the query above is run and a concurrent view drop happens
	// just before the pg_get_viewdef function execute. Also, error
	// out if a view has anyarray typecasts in their view definition
	// as those will error out during restore. Anyarray typecasts can
	// show up if the view was created with array typecasting in an
	// OpExpr (Operator Expression) argument.
	verifiedResults := make([]View, 0)
	for _, result := range results {
		if result.Definition.Valid {
			if strings.Contains(result.Definition.String, "::anyarray") {
				gplog.Fatal(errors.Errorf("Detected anyarray type cast in view definition for View '%s'", result.FQN()),
					"Drop the view or recreate the view without explicit array type casts.")
			}
		} else {
			// do not append views with invalid definitions
			gplog.Warn("View '%s.%s' not backed up, most likely dropped after gpbackup had begun.", result.Schema, result.Name)
			continue
		}

		if result.IsMaterialized {
			result.DistPolicy = distPolicies[result.Oid]
		}
		result.ColumnDefs = columnDefs[result.Oid]
		verifiedResults = append(verifiedResults, result)
	}

	return verifiedResults
}

// This function is responsible for getting the necessary access share
// locks for the target relations. This is mainly to protect the metadata
// dumping part but it also makes the main worker thread (worker 0) the
// most resilient for the later data dumping logic. Locks will still be
// taken for --data-only calls.
func LockTables(connectionPool *dbconn.DBConn, tables []Relation) {
	gplog.Info("Acquiring ACCESS SHARE locks on tables")

	progressBar := utils.NewProgressBar(len(tables), "Locks acquired: ", utils.PB_VERBOSE)
	progressBar.Start()
	var lockMode string
	const batchSize = 100
	lastBatchSize := len(tables) % batchSize
	tableBatches := GenerateTableBatches(tables, batchSize)
	currentBatchSize := batchSize
	if connectionPool.Version.AtLeast("7") {
		lockMode = `IN ACCESS SHARE MODE COORDINATOR ONLY`
	} else if connectionPool.Version.AtLeast("6.21.0") {
		lockMode = `IN ACCESS SHARE MODE MASTER ONLY`
	} else {
		lockMode = `IN ACCESS SHARE MODE`
	}
	// The LOCK TABLE query could block if someone else is holding an
	// AccessExclusiveLock on the table.  In the case gpbackup is interrupted,
	// cancelBlockedQueries() will cancel these queries during cleanup.
	for i, currentBatch := range tableBatches {
		_, err := connectionPool.Exec(fmt.Sprintf("LOCK TABLE %s %s", currentBatch, lockMode))
		if err != nil {
			if wasTerminated {
				gplog.Warn("Interrupt received while acquiring ACCESS SHARE locks on tables")
				select {} // wait for cleanup thread to exit gpbackup
			} else {
				gplog.FatalOnError(err)
			}
		}
		if i == len(tableBatches)-1 && lastBatchSize > 0 {
			currentBatchSize = lastBatchSize
		}
		progressBar.Add(currentBatchSize)
	}

	progressBar.Finish()
}

// ChangedRelation is a relation whose storage no longer matches what the
// backup snapshot describes: it was rewritten, truncated, or dropped (and
// possibly recreated under the same name) after the snapshot was taken. It is
// either one of the locked tables or a partition below one of them; Ancestors
// lists the inheritance chain up to the locked table, nearest first.
type ChangedRelation struct {
	Relation
	Dropped   bool
	Ancestors []Relation
}

// Reason says what happened to the relation, for warnings.
func (c ChangedRelation) Reason() string {
	if c.Dropped {
		return "dropped and recreated"
	}
	return "rewritten or truncated"
}

// Describe names the relation, its Reason, and the table it is a partition of.
func (c ChangedRelation) Describe() string {
	if len(c.Ancestors) > 0 {
		root := c.Ancestors[len(c.Ancestors)-1]
		return fmt.Sprintf("%s (%s, partition of %s)", c.FQN(), c.Reason(), root.FQN())
	}
	return fmt.Sprintf("%s (%s)", c.FQN(), c.Reason())
}

// The storage check groups this many locked tables per query. Each batch is
// one SELECT, so the size bounds the statement, not the result: the partitions
// below the batch come back whatever the size is. LockTables uses smaller
// batches only because each of its statements is a separate round trip that
// advances a progress bar.
const changedRelationsBatchSize = 1000

/*
 * GetRelationsChangedSinceSnapshot reports which of the given locked tables,
 * or of the partitions below them, changed on disk between the backup
 * snapshot and the locks now held.
 *
 * The catalog rows come from the backup snapshot, while pg_relation_filenode()
 * looks the relation up in the live catalog, the same way LOCK TABLE and the
 * later COPY do. A table rewritten by ALTER TABLE ... SET WITH (REORGANIZE=true)
 * or SET DISTRIBUTED BY, a heap table rewritten by VACUUM FULL, or a truncated
 * table has a new relfilenode; a table dropped (and possibly recreated under
 * the same name) has none. Either way the snapshot can no longer see the
 * table's data: the old files are gone, and for append-optimized tables the
 * pg_aoseg relation named under the snapshot no longer exists.
 *
 * Partitions are followed through pg_inherits because a backup that does not
 * use --leaf-partition-data locks and copies the parent only, while the data
 * lives in the children; LOCK TABLE on the parent locks them too, so the
 * check is race-free for them as well. Relations without storage (relfilenode
 * 0, e.g. partition roots) are not compared. Locked tables come back in input
 * order, each followed by its changed partitions.
 */
func GetRelationsChangedSinceSnapshot(connectionPool *dbconn.DBConn, tables []Relation) []ChangedRelation {
	if len(tables) == 0 {
		return nil
	}
	type relationStorage struct {
		Oid                 uint32
		ParentOid           sql.NullInt64
		Schema              string
		Name                string
		SnapshotRelfilenode uint32
		CurrentRelfilenode  sql.NullInt64
	}
	rows := make([]relationStorage, 0)
	for start := 0; start < len(tables); start += changedRelationsBatchSize {
		end := start + changedRelationsBatchSize
		if end > len(tables) {
			end = len(tables)
		}
		oids := make([]string, 0, end-start)
		for _, table := range tables[start:end] {
			oids = append(oids, strconv.FormatUint(uint64(table.Oid), 10))
		}
		query := fmt.Sprintf(`
	WITH RECURSIVE inheritance(oid, parentoid) AS (
		SELECT c.oid, NULL::oid
		FROM pg_class c
		WHERE c.oid IN (%s)
		UNION ALL
		SELECT i.inhrelid, i.inhparent
		FROM pg_inherits i
			JOIN inheritance h ON i.inhparent = h.oid
	)
	SELECT DISTINCT h.oid,
		h.parentoid,
		quote_ident(n.nspname) AS schema,
		quote_ident(c.relname) AS name,
		c.relfilenode AS snapshotrelfilenode,
		pg_catalog.pg_relation_filenode(c.oid) AS currentrelfilenode
	FROM inheritance h
		JOIN pg_class c ON c.oid = h.oid
		JOIN pg_namespace n ON n.oid = c.relnamespace`, strings.Join(oids, ", "))
		batchRows := make([]relationStorage, 0)
		err := connectionPool.Select(&batchRows, query)
		gplog.FatalOnError(err)
		rows = append(rows, batchRows...)
	}

	locked := make(map[uint32]int, len(tables))
	for i, table := range tables {
		locked[table.Oid] = i
	}
	relations := make(map[uint32]Relation)
	parents := make(map[uint32]uint32)
	dropped := make(map[uint32]bool)
	for _, row := range rows {
		if _, isLocked := locked[row.Oid]; isLocked {
			relations[row.Oid] = tables[locked[row.Oid]]
		} else {
			relations[row.Oid] = Relation{Oid: row.Oid, Schema: row.Schema, Name: row.Name}
		}
		// A partition that is itself locked appears both on its own and
		// below its parent; keep the parent link.
		if row.ParentOid.Valid {
			parents[row.Oid] = uint32(row.ParentOid.Int64)
		}
		if row.SnapshotRelfilenode == 0 {
			continue
		}
		if !row.CurrentRelfilenode.Valid {
			dropped[row.Oid] = true
		} else if uint32(row.CurrentRelfilenode.Int64) != row.SnapshotRelfilenode {
			dropped[row.Oid] = false
		}
	}

	ancestorsOf := func(oid uint32) []Relation {
		ancestors := make([]Relation, 0)
		for parent, ok := parents[oid]; ok; parent, ok = parents[parent] {
			ancestors = append(ancestors, relations[parent])
		}
		return ancestors
	}
	rootIndex := func(oid uint32) int {
		root := oid
		for parent, ok := parents[root]; ok; parent, ok = parents[root] {
			root = parent
		}
		return locked[root]
	}

	changed := make([]ChangedRelation, 0)
	for oid, isDropped := range dropped {
		changed = append(changed, ChangedRelation{Relation: relations[oid], Dropped: isDropped, Ancestors: ancestorsOf(oid)})
	}
	sort.Slice(changed, func(i, j int) bool {
		ri, rj := rootIndex(changed[i].Oid), rootIndex(changed[j].Oid)
		if ri != rj {
			return ri < rj
		}
		if len(changed[i].Ancestors) != len(changed[j].Ancestors) {
			return len(changed[i].Ancestors) < len(changed[j].Ancestors)
		}
		return changed[i].Oid < changed[j].Oid
	})
	return changed
}

// GenerateTableBatches batches tables to reduce network congestion and
// resource contention.  Returns an array of batches where a batch of tables is
// a single string with comma separated tables
func GenerateTableBatches(tables []Relation, batchSize int) []string {
	var tableNames []string
	for _, table := range tables {
		tableNames = append(tableNames, table.FQN())
	}

	var end int
	var batches []string
	i := 0
	for i < len(tables) {
		if i+batchSize < len(tables) {
			end = i + batchSize
		} else {
			end = len(tables)
		}

		batches = append(batches, strings.Join(tableNames[i:end], ", "))
		i = end
	}

	return batches
}

// configDumpRelation extends Relation with the optional WHERE condition from
// pg_extension_config_dump, used to filter out rows that the extension's
// install script inserts by default (and that CREATE EXTENSION will re-create
// on restore).
type configDumpRelation struct {
	Relation
	FilterCond string
}

// GetExtensionConfigDumpRelations returns Relation objects and their filter
// conditions for tables registered via pg_extension_config_dump. These are
// extension-owned tables whose data should be backed up (and restored)
// alongside user data, even though the table DDL is managed by the extension
// itself.
//
// This mirrors the behavior of pg_dump, which queries pg_extension.extconfig
// and pg_extension.extcondition to identify config dump tables and applies any
// filter conditions so that only user-modified rows are dumped. Without the
// filter, rows inserted by the extension's install script would be duplicated
// on restore (since CREATE EXTENSION re-inserts them).
//
// The returned filter conditions map contains entries only for tables that have
// a non-empty WHERE condition. The caller is responsible for building full
// Table objects (via ConstructDefinitionsForTables) and attaching the filter
// conditions as ExtConfigFilterCond.
func GetExtensionConfigDumpRelations(connectionPool *dbconn.DBConn) ([]Relation, map[uint32]string) {
	query := fmt.Sprintf(`
	SELECT n.oid AS schemaoid,
		c.oid AS oid,
		quote_ident(n.nspname) AS schema,
		quote_ident(c.relname) AS name,
		COALESCE(cond.filtercond, '') AS filtercond
	FROM pg_catalog.pg_extension e,
		unnest(e.extconfig, e.extcondition) AS cond(config_oid, filtercond)
		JOIN pg_catalog.pg_class c ON c.oid = cond.config_oid
		JOIN pg_catalog.pg_namespace n ON c.relnamespace = n.oid
	WHERE e.extconfig IS NOT NULL
		AND %s`, relationAndSchemaFilterClause())

	configRelations := make([]configDumpRelation, 0)
	err := connectionPool.Select(&configRelations, query)
	gplog.FatalOnError(err)

	if len(configRelations) == 0 {
		return nil, nil
	}

	gplog.Info("Found %d extension config dump table(s)", len(configRelations))

	filterCondByOid := make(map[uint32]string, len(configRelations))
	plainRelations := make([]Relation, len(configRelations))
	for i, cr := range configRelations {
		plainRelations[i] = cr.Relation
		if cr.FilterCond != "" {
			filterCondByOid[cr.Relation.Oid] = cr.FilterCond
		}
	}

	return plainRelations, filterCondByOid
}
