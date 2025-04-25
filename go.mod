module github.com/greenplum-db/gpbackup

go 1.23

require (
	github.com/DATA-DOG/go-sqlmock v1.5.0
	github.com/blang/semver v3.5.1+incompatible
	github.com/blang/vfs v1.0.0
	github.com/greenplum-db/gp-common-go-libs v1.0.19
	github.com/jackc/pgconn v1.14.3
	github.com/jmoiron/sqlx v1.3.5
	github.com/klauspost/compress v1.15.15
	github.com/lib/pq v1.10.7
	github.com/mattn/go-sqlite3 v1.14.19
	github.com/nightlyone/lockfile v1.0.0
	github.com/onsi/ginkgo/v2 v2.23.3
	github.com/onsi/gomega v1.37.0
	github.com/pkg/errors v0.9.1
	github.com/sergi/go-diff v1.3.1
	github.com/spf13/cobra v1.6.1
	github.com/spf13/pflag v1.0.5
	golang.org/x/sys v0.32.0
	golang.org/x/tools v0.30.0
	gopkg.in/cheggaaa/pb.v1 v1.0.28
	gopkg.in/yaml.v2 v2.4.0
)

replace github.com/greenplum-db/gp-common-go-libs => github.com/warehouse-pg/gp-common-go-libs v1.0.23-0.20250428111627-52ae4fabda2f

require (
	github.com/fatih/color v1.14.1 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20241210010833-40e02aabc2ad // indirect
	github.com/inconshreveable/mousetrap v1.0.1 // indirect
	github.com/jackc/chunkreader/v2 v2.0.1 // indirect
	github.com/jackc/pgio v1.0.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgproto3/v2 v2.3.3 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.7.4 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-runewidth v0.0.13 // indirect
	github.com/rivo/uniseg v0.2.0 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/mod v0.23.0 // indirect
	golang.org/x/net v0.39.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/text v0.24.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
