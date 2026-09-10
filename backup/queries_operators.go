package backup

/*
 * This file contains structs and functions related to executing specific
 * queries to gather metadata for the objects handled in predata_general.go.
 */

import (
	"fmt"
	"sort"

	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/gplog"
)

type Operator struct {
	Oid              uint32
	Schema           string
	Name             string
	Procedure        string
	LeftArgType      string
	RightArgType     string
	CommutatorOp     string
	NegatorOp        string
	RestrictFunction string
	JoinFunction     string
	CanHash          bool
	CanMerge         bool
}

func (o Operator) GetMetadataEntry() (string, toc.MetadataEntry) {
	return "predata",
		toc.MetadataEntry{
			Schema:          o.Schema,
			Name:            o.Name,
			ObjectType:      toc.OBJ_OPERATOR,
			ReferenceObject: "",
			StartByte:       0,
			EndByte:         0,
		}
}

func (o Operator) GetUniqueID() UniqueID {
	return UniqueID{ClassID: PG_OPERATOR_OID, Oid: o.Oid}
}

func (o Operator) FQN() string {
	leftArg := "NONE"
	rightArg := "NONE"
	if o.LeftArgType != "-" {
		leftArg = o.LeftArgType
	}
	if o.RightArgType != "-" {
		rightArg = o.RightArgType
	}
	return fmt.Sprintf("%s.%s (%s, %s)", o.Schema, o.Name, leftArg, rightArg)
}

func GetOperators(connectionPool *dbconn.DBConn) []Operator {
	results := make([]Operator, 0)
	query := fmt.Sprintf(`
	SELECT o.oid AS oid,
		quote_ident(n.nspname) AS schema,
		oprname AS name,
		oprcode::regproc AS procedure,
		oprleft::regtype AS leftargtype,
		oprright::regtype AS rightargtype,
		oprcom::regoper AS commutatorop,
		oprnegate::regoper AS negatorop,
		oprrest AS restrictfunction,
		oprjoin AS joinfunction,
		oprcanmerge AS canmerge,
		oprcanhash AS canhash
	FROM pg_operator o
		JOIN pg_namespace n on n.oid = o.oprnamespace
	WHERE %s AND oprcode != 0
		AND %s`, SchemaFilterClause("n"), ExtensionFilterClause("o"))

	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	return results
}

type OperatorFamily struct {
	Oid         uint32
	Schema      string
	Name        string
	IndexMethod string
}

func (opf OperatorFamily) GetMetadataEntry() (string, toc.MetadataEntry) {
	return "predata",
		toc.MetadataEntry{
			Schema:          opf.Schema,
			Name:            opf.Name,
			ObjectType:      toc.OBJ_OPERATOR_FAMILY,
			ReferenceObject: "",
			StartByte:       0,
			EndByte:         0,
		}
}

func (opf OperatorFamily) GetUniqueID() UniqueID {
	return UniqueID{ClassID: PG_OPFAMILY_OID, Oid: opf.Oid}
}

func (opf OperatorFamily) FQN() string {
	return fmt.Sprintf("%s USING %s", utils.MakeFQN(opf.Schema, opf.Name), opf.IndexMethod)
}

func GetOperatorFamilies(connectionPool *dbconn.DBConn) []OperatorFamily {
	results := make([]OperatorFamily, 0)
	query := fmt.Sprintf(`
	SELECT o.oid AS oid,
		quote_ident(n.nspname) AS schema,
		quote_ident(opfname) AS name,
		(SELECT quote_ident(amname) FROM pg_am WHERE oid = opfmethod) AS indexMethod
	FROM pg_opfamily o
		JOIN pg_namespace n on n.oid = o.opfnamespace
	WHERE %s
		AND %s`,
		SchemaFilterClause("n"), ExtensionFilterClause("o"))
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	return results
}

type OperatorClass struct {
	Oid          uint32
	Schema       string
	Name         string
	FamilySchema string
	FamilyName   string
	IndexMethod  string
	Type         string
	Default      bool
	StorageType  string
	Operators    []OperatorClassOperator
	Functions    []OperatorClassFunction
}

func (opc OperatorClass) GetMetadataEntry() (string, toc.MetadataEntry) {
	return "predata",
		toc.MetadataEntry{
			Schema:          opc.Schema,
			Name:            opc.Name,
			ObjectType:      toc.OBJ_OPERATOR_CLASS,
			ReferenceObject: "",
			StartByte:       0,
			EndByte:         0,
		}
}

func (opc OperatorClass) GetUniqueID() UniqueID {
	return UniqueID{ClassID: PG_OPCLASS_OID, Oid: opc.Oid}
}

func (opc OperatorClass) FQN() string {
	return fmt.Sprintf("%s USING %s", utils.MakeFQN(opc.Schema, opc.Name), opc.IndexMethod)
}

func GetOperatorClasses(connectionPool *dbconn.DBConn) []OperatorClass {
	results := make([]OperatorClass, 0)
	query := fmt.Sprintf(`
	SELECT c.oid AS oid,
		quote_ident(cls_ns.nspname) AS schema,
		quote_ident(opcname) AS name,
		quote_ident(fam_ns.nspname) AS familyschema,
		quote_ident(opfname) AS familyname,
		(SELECT amname FROM pg_catalog.pg_am WHERE oid = opcmethod) AS indexmethod,
		opcintype::pg_catalog.regtype AS type,
		opcdefault AS default,
		opckeytype::pg_catalog.regtype AS storagetype
	FROM pg_catalog.pg_opclass c
		LEFT JOIN pg_catalog.pg_opfamily f ON f.oid = opcfamily
		JOIN pg_catalog.pg_namespace cls_ns ON cls_ns.oid = opcnamespace
		JOIN pg_catalog.pg_namespace fam_ns ON fam_ns.oid = opfnamespace
	WHERE %s
		AND %s`,
		SchemaFilterClause("cls_ns"), ExtensionFilterClause("c"))

	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	operators := GetOperatorClassOperators(connectionPool)
	for i := range results {
		results[i].Operators = operators[results[i].Oid]
	}
	functions := GetOperatorClassFunctions(connectionPool)
	for i := range results {
		results[i].Functions = functions[results[i].Oid]
	}

	return results
}

type OperatorClassOperator struct {
	ClassOid       uint32
	StrategyNumber int
	Operator       string
	Recheck        bool
	OrderByFamily  string
}

func GetOperatorClassOperators(connectionPool *dbconn.DBConn) map[uint32][]OperatorClassOperator {
	results := make([]OperatorClassOperator, 0)
	version5Query := `
	SELECT refobjid AS classoid,
		amopstrategy AS strategynumber,
		amopopr::pg_catalog.regoperator AS operator,
		amopreqcheck AS recheck
	FROM pg_catalog.pg_amop ao
		JOIN pg_catalog.pg_depend d ON d.objid = ao.oid
	WHERE refclassid = 'pg_catalog.pg_opclass'::pg_catalog.regclass
		AND classid = 'pg_catalog.pg_amop'::pg_catalog.regclass
	ORDER BY amopstrategy`

	atLeast6Query := `
	SELECT refobjid AS classoid,
		amopstrategy AS strategynumber,
		amopopr::pg_catalog.regoperator AS operator,
		coalesce(quote_ident(ns.nspname) || '.' || quote_ident(opf.opfname), '') AS orderbyfamily
	FROM pg_catalog.pg_amop ao
		JOIN pg_catalog.pg_depend d ON d.objid = ao.oid
		LEFT JOIN pg_opfamily opf ON opf.oid = ao.amopsortfamily
		LEFT JOIN pg_namespace ns ON ns.oid = opf.opfnamespace
	WHERE refclassid = 'pg_catalog.pg_opclass'::pg_catalog.regclass
		AND classid = 'pg_catalog.pg_amop'::pg_catalog.regclass
	ORDER BY amopstrategy`

	query := ""
	if connectionPool.Version.Is("5") {
		query = version5Query
	} else {
		query = atLeast6Query
	}

	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	// PG13+ (WHPG19) records a GiST/GIN/SP-GiST opclass's OPERATOR members as
	// depending on the operator FAMILY even when they were declared inline in
	// CREATE OPERATOR CLASS -- see the comment above the operator loop in
	// gistvalidate(). The pg_depend query above therefore returns nothing for
	// them, and since gpbackup only ever emits a bare CREATE OPERATOR FAMILY
	// and never dumps family members, those operators would be lost outright.
	// Recover them from the family, and merge rather than replace: btree and
	// hash still depend on the opclass (nbtvalidate/hashvalidate), and that
	// attribution is exact where it exists.
	if connectionPool.Version.AtLeast("19") {
		familyResults := make([]OperatorClassOperator, 0)
		err = connectionPool.Select(&familyResults, familyMemberQuery(`
		amopstrategy AS strategynumber,
		amopopr::pg_catalog.regoperator AS operator,
		coalesce(quote_ident(sort_ns.nspname) || '.' || quote_ident(sort_opf.opfname), '') AS orderbyfamily`,
			`JOIN pg_catalog.pg_amop ao ON ao.amopfamily = cls.opcfamily
			AND ao.amoplefttype = cls.opcintype AND ao.amoprighttype = cls.opcintype
		LEFT JOIN pg_catalog.pg_opfamily sort_opf ON sort_opf.oid = ao.amopsortfamily
		LEFT JOIN pg_catalog.pg_namespace sort_ns ON sort_ns.oid = sort_opf.opfnamespace`))
		gplog.FatalOnError(err)
		results = append(results, familyResults...)
	}

	operators := make(map[uint32][]OperatorClassOperator)
	seen := make(map[OperatorClassOperator]bool)
	for _, result := range results {
		// A class whose members pg_depend already attributed exactly will match
		// the family lookup as well; keep the first sighting of each member.
		if seen[result] {
			continue
		}
		seen[result] = true
		operators[result.ClassOid] = append(operators[result.ClassOid], result)
	}
	for classOid := range operators {
		sort.SliceStable(operators[classOid], func(i, j int) bool {
			return operators[classOid][i].StrategyNumber < operators[classOid][j].StrategyNumber
		})
	}
	return operators
}

// familyMemberQuery builds the WHPG19 fallback that attributes an operator
// family's members to the one opclass that family belongs to.
//
// It deliberately covers only families holding a single opclass -- the shape
// CREATE OPERATOR CLASS produces when no FAMILY is named. In a family shared by
// several classes there is nothing in the catalog that says which class a
// family-level member was declared with, and guessing would fold members that
// belong to a sibling class (or that were added by ALTER OPERATOR FAMILY) into
// this one: on restore those come back as class members, and if the family
// already held a matching member the restore fails outright with "operator
// already exists in operator family". Losing them, as gpbackup already does
// today for family members generally, is the safer direction.
//
// The schema and extension filters matter for more than tidiness: built-in
// catalog members are pinned and have no pg_depend rows, so the query they
// supplement never returned them. Without the filters every backup would fetch
// the entire catalog's members only for GetOperatorClasses to discard them.
func familyMemberQuery(selectList string, joins string) string {
	return fmt.Sprintf(`
	SELECT cls.oid AS classoid,
		%s
	FROM pg_catalog.pg_opclass cls
		JOIN pg_catalog.pg_namespace cls_ns ON cls_ns.oid = cls.opcnamespace
		%s
	WHERE %s
		AND %s
		AND NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_opclass other
			WHERE other.opcfamily = cls.opcfamily AND other.oid <> cls.oid)`,
		selectList, joins, SchemaFilterClause("cls_ns"), ExtensionFilterClause("cls"))
}

type OperatorClassFunction struct {
	ClassOid      uint32
	SupportNumber int
	FunctionName  string
	LeftType      string `db:"amproclefttype"`
	RightType     string `db:"amprocrighttype"`
}

func GetOperatorClassFunctions(connectionPool *dbconn.DBConn) map[uint32][]OperatorClassFunction {
	results := make([]OperatorClassFunction, 0)
	query := `
	SELECT refobjid AS classoid,
		amprocnum AS supportnumber,
		amproclefttype::regtype,
		amprocrighttype::regtype,
		amproc::regprocedure::text AS functionname
	FROM pg_catalog.pg_amproc ap
		JOIN pg_catalog.pg_depend d ON d.objid = ap.oid
	WHERE refclassid = 'pg_catalog.pg_opclass'::pg_catalog.regclass
		AND classid = 'pg_catalog.pg_amproc'::pg_catalog.regclass
	ORDER BY amprocnum`

	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	// Unlike the OPERATOR members, a GiST opclass's *required* support
	// functions keep a hard dependency on the opclass, so the query above
	// still finds those on WHPG19; only the optional ones (compress, distance,
	// options, ...) are forced onto the family. Merge those in the same way,
	// rather than replacing a query that is exact for everything else.
	if connectionPool.Version.AtLeast("19") {
		familyResults := make([]OperatorClassFunction, 0)
		err = connectionPool.Select(&familyResults, familyMemberQuery(`
		amprocnum AS supportnumber,
		amproclefttype::regtype,
		amprocrighttype::regtype,
		amproc::regprocedure::text AS functionname`,
			`JOIN pg_catalog.pg_amproc ap ON ap.amprocfamily = cls.opcfamily
			AND ap.amproclefttype = cls.opcintype AND ap.amprocrighttype = cls.opcintype`))
		gplog.FatalOnError(err)
		results = append(results, familyResults...)
	}

	functions := make(map[uint32][]OperatorClassFunction)
	seen := make(map[OperatorClassFunction]bool)
	for _, result := range results {
		if seen[result] {
			continue
		}
		seen[result] = true
		functions[result.ClassOid] = append(functions[result.ClassOid], result)
	}
	for classOid := range functions {
		sort.SliceStable(functions[classOid], func(i, j int) bool {
			return functions[classOid][i].SupportNumber < functions[classOid][j].SupportNumber
		})
	}
	return functions
}
