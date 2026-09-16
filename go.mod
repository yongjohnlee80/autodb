module github.com/yongjohnlee80/autodb

go 1.25.3

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/coder/websocket v1.8.15
	github.com/go-sql-driver/mysql v1.10.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/jmoiron/sqlx v1.4.0
	github.com/lib/pq v1.12.3
	github.com/yongjohnlee80/golib v0.5.22
	golang.org/x/crypto v0.55.0
	golang.org/x/term v0.45.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.52.0 // indirect
)

// TEMPORARY, MUST NOT MERGE AS IT STANDS. The pinned connection's explicit
// physical destruction (postgres.Destroyer) is committed on golib's
// pinned-conn-destroy branch and is in no tagged release yet, so this branch is
// built against a sibling checkout. Replace this directive with a real version
// — the first golib tag that carries Destroyer — before merging. The path is
// relative and points at golib's pinned-conn-destroy worktree; it resolves nowhere
// else, which is the point: CI fails loudly rather than building the wrong golib.
replace github.com/yongjohnlee80/golib => ../../golib/pinned-conn-destroy
