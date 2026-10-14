package backup

import (
	"fmt"

	"github.com/greenplum-db/gpbackup/toc"
	"github.com/warehouse-pg/common-go-libs/dbconn"
	"github.com/warehouse-pg/common-go-libs/gplog"
)

func GetAOIncrementalMetadata(connectionPool *dbconn.DBConn) map[string]toc.AOEntry {
	gplog.Verbose("Querying table row mod counts")
	var modCounts = getAllModCounts(connectionPool)
	gplog.Verbose("Querying last DDL modification timestamp for tables")
	var lastDDLTimestamps = getLastDDLTimestamps(connectionPool)
	aoTableEntries := make(map[string]toc.AOEntry)
	for aoTableFQN := range modCounts {
		aoTableEntries[aoTableFQN] = toc.AOEntry{
			Modcount:         modCounts[aoTableFQN],
			LastDDLTimestamp: lastDDLTimestamps[aoTableFQN],
		}
	}

	return aoTableEntries
}

// aoSegTable identifies the AO segment table holding an AO table's modcount,
// and whether the AO table it belongs to is coordinator-only, which determines
// where that modcount has to be read from.
type aoSegTable struct {
	FQN               string
	IsCoordinatorOnly bool
}

func getAllModCounts(connectionPool *dbconn.DBConn) map[string]int64 {
	var segTableFQNs = getAOSegTableFQNs(connectionPool)
	modCounts := make(map[string]int64)
	for aoTableFQN, segTableFQN := range segTableFQNs {
		// A relation that changed on disk since the snapshot, or a table whose
		// data was left out because a partition did, gets no incremental
		// entry: its data is not backed up, and a changed relation's segment
		// relation name comes from the snapshot and may no longer exist.
		if changedRelations[aoTableFQN] {
			continue
		}
		modCounts[aoTableFQN] = getModCount(connectionPool, segTableFQN)
	}
	return modCounts
}

func getAOSegTableFQNs(connectionPool *dbconn.DBConn) map[string]aoSegTable {

	before7Query := fmt.Sprintf(`
		SELECT seg.aotablefqn,
			'pg_aoseg.' || quote_ident(aoseg_c.relname) AS aosegtablefqn,
			false AS iscoordinatoronly
		FROM pg_class aoseg_c
			JOIN (SELECT pg_ao.relid AS aooid,
					pg_ao.segrelid,
					aotables.aotablefqn,
					aotables.iscoordinatoronly
				FROM pg_appendonly pg_ao
					JOIN (SELECT c.oid,
							quote_ident(n.nspname)|| '.' || quote_ident(c.relname) AS aotablefqn,
							false AS iscoordinatoronly
						FROM pg_class c
							JOIN pg_namespace n ON c.relnamespace = n.oid
						WHERE relstorage IN ( 'ao', 'co' )
							AND %s
					) aotables ON pg_ao.relid = aotables.oid
			) seg ON aoseg_c.oid = seg.segrelid`, relationAndSchemaFilterClause())

	// A coordinator-only table's AO segment data lives on the coordinator, so
	// its modcount has to be read locally rather than from the segments.
	atLeast7Query := fmt.Sprintf(`
		SELECT seg.aotablefqn,
			'pg_aoseg.' || quote_ident(aoseg_c.relname) AS aosegtablefqn,
			seg.iscoordinatoronly
		FROM pg_class aoseg_c
			JOIN (SELECT pg_ao.relid AS aooid,
					pg_ao.segrelid,
					aotables.aotablefqn,
					aotables.iscoordinatoronly
				FROM pg_appendonly pg_ao
					JOIN (SELECT c.oid,
							quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS aotablefqn,
							COALESCE(d.policytype = 'e', false) AS iscoordinatoronly
						FROM pg_class c
							JOIN pg_namespace n ON c.relnamespace = n.oid
							JOIN pg_am a ON c.relam = a.oid
							LEFT JOIN gp_distribution_policy d ON d.localoid = c.oid
						WHERE a.amname in ('ao_row', 'ao_column')
							AND %s
					) aotables ON pg_ao.relid = aotables.oid
			) seg ON aoseg_c.oid = seg.segrelid`, relationAndSchemaFilterClause())

	query := ""
	if connectionPool.Version.Before("7") {
		query = before7Query
	} else {
		query = atLeast7Query
	}

	results := make([]struct {
		AOTableFQN        string
		AOSegTableFQN     string
		IsCoordinatorOnly bool
	}, 0)
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	resultMap := make(map[string]aoSegTable)
	for _, result := range results {
		resultMap[result.AOTableFQN] = aoSegTable{
			FQN:               result.AOSegTableFQN,
			IsCoordinatorOnly: result.IsCoordinatorOnly,
		}
	}
	return resultMap
}

func getModCount(connectionPool *dbconn.DBConn, segTable aoSegTable) int64 {

	localQuery := fmt.Sprintf(`SELECT COALESCE(pg_catalog.sum(modcount), 0) AS modcount FROM %s`,
		segTable.FQN)

	// In GPDB 7+, the coordinator no longer stores AO segment data so we must
	// query the modcount from the segments. Unfortunately, this does give a
	// false positive if a VACUUM FULL compaction happens on the AO table.
	atLeast7Query := fmt.Sprintf(`SELECT COALESCE(pg_catalog.sum(modcount), 0) AS modcount FROM gp_dist_random('%s')`,
		segTable.FQN)

	query := ""
	if connectionPool.Version.Before("7") || segTable.IsCoordinatorOnly {
		// A coordinator-only table has no AO segment rows on the segments, so
		// reading them there would report a modcount of 0 forever and every
		// incremental backup would skip the table's changed data.
		query = localQuery
	} else {
		query = atLeast7Query
	}

	var results []struct {
		Modcount int64
	}
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	return results[0].Modcount
}

func getLastDDLTimestamps(connectionPool *dbconn.DBConn) map[string]string {
	before7Query := fmt.Sprintf(`
		SELECT quote_ident(aoschema) || '.' || quote_ident(aorelname) as aotablefqn,
			lastddltimestamp
		FROM ( SELECT c.oid AS aooid,
					n.nspname AS aoschema,
					c.relname AS aorelname
				FROM pg_class c
				JOIN pg_namespace n ON c.relnamespace = n.oid
				WHERE c.relstorage IN ('ao', 'co')
				AND %s
			) aotables
		JOIN ( SELECT lo.objid,
					MAX(lo.statime) AS lastddltimestamp
				FROM pg_stat_last_operation lo
				WHERE lo.staactionname IN ('CREATE', 'ALTER', 'TRUNCATE')
				GROUP BY lo.objid
			) lastop
		ON aotables.aooid = lastop.objid`, relationAndSchemaFilterClause())

	atLeast7Query := fmt.Sprintf(`
		SELECT quote_ident(aoschema) || '.' || quote_ident(aorelname) as aotablefqn,
			lastddltimestamp
		FROM ( SELECT c.oid AS aooid,
					n.nspname AS aoschema,
					c.relname AS aorelname
				FROM pg_class c
					JOIN pg_namespace n ON c.relnamespace = n.oid
					JOIN pg_am a ON c.relam = a.oid
				WHERE a.amname in ('ao_row', 'ao_column')
					AND %s
			) aotables
		JOIN ( SELECT lo.objid,
					MAX(lo.statime) AS lastddltimestamp
				FROM pg_stat_last_operation lo
				WHERE lo.staactionname IN ('CREATE', 'ALTER', 'TRUNCATE')
				GROUP BY lo.objid
			) lastop
		ON aotables.aooid = lastop.objid`, relationAndSchemaFilterClause())

	query := ""
	if connectionPool.Version.Before("7") {
		query = before7Query
	} else {
		query = atLeast7Query
	}

	var results []struct {
		AOTableFQN       string
		LastDDLTimestamp string
	}
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	resultMap := make(map[string]string)
	for _, result := range results {
		resultMap[result.AOTableFQN] = result.LastDDLTimestamp
	}
	return resultMap
}
