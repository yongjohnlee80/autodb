package exec

// PostgreSQL keywords that cannot be used as a bare identifier.
//
// A savepoint name is a ColId, which PostgreSQL's grammar defines as an
// identifier, an unreserved keyword or a col_name keyword — but NOT a
// reserved or type/function-name keyword. So `ROLLBACK TO COMMIT` names a
// savepoint called "commit" and parses, while `ROLLBACK TO SELECT` is a
// syntax error. Quoting lifts the restriction: `ROLLBACK TO "SELECT"` is
// fine, because a delimited identifier is never a keyword.
//
// Treating every word as a possible name meant SELECT and AND were answered
// with "savepoints are not implemented" — a missing feature, for what the
// server calls a syntax error.
//
// GENERATED from the server rather than transcribed, because a
// hand-maintained keyword list is a hand-maintained mistake:
//
//	SELECT upper(word) FROM pg_get_keywords()
//	WHERE catcode IN ('R','T') ORDER BY 1;
//
// against PostgreSQL 17.6 (101 words). TestReservedKeywords_MatchTheServer
// re-derives it whenever a live server is configured, so it cannot drift
// unnoticed.
//
// The rule is PostgreSQL's, which is the product ruling for session-capable
// connections. A MySQL-reserved word that PostgreSQL leaves unreserved would
// be admitted here as a name — and since EVERY well-formed savepoint target
// ends in the same refusal, that changes which refusal an exotic MySQL name
// receives, not whether it is refused.
var pgReservedKeywords = map[string]bool{
	"ALL": true, "ANALYSE": true, "ANALYZE": true, "AND": true,
	"ANY": true, "ARRAY": true, "AS": true, "ASC": true,
	"ASYMMETRIC": true, "AUTHORIZATION": true, "BINARY": true, "BOTH": true,
	"CASE": true, "CAST": true, "CHECK": true, "COLLATE": true,
	"COLLATION": true, "COLUMN": true, "CONCURRENTLY": true, "CONSTRAINT": true,
	"CREATE": true, "CROSS": true, "CURRENT_CATALOG": true, "CURRENT_DATE": true,
	"CURRENT_ROLE": true, "CURRENT_SCHEMA": true, "CURRENT_TIME": true, "CURRENT_TIMESTAMP": true,
	"CURRENT_USER": true, "DEFAULT": true, "DEFERRABLE": true, "DESC": true,
	"DISTINCT": true, "DO": true, "ELSE": true, "END": true,
	"EXCEPT": true, "FALSE": true, "FETCH": true, "FOR": true,
	"FOREIGN": true, "FREEZE": true, "FROM": true, "FULL": true,
	"GRANT": true, "GROUP": true, "HAVING": true, "ILIKE": true,
	"IN": true, "INITIALLY": true, "INNER": true, "INTERSECT": true,
	"INTO": true, "IS": true, "ISNULL": true, "JOIN": true,
	"LATERAL": true, "LEADING": true, "LEFT": true, "LIKE": true,
	"LIMIT": true, "LOCALTIME": true, "LOCALTIMESTAMP": true, "NATURAL": true,
	"NOT": true, "NOTNULL": true, "NULL": true, "OFFSET": true,
	"ON": true, "ONLY": true, "OR": true, "ORDER": true,
	"OUTER": true, "OVERLAPS": true, "PLACING": true, "PRIMARY": true,
	"REFERENCES": true, "RETURNING": true, "RIGHT": true, "SELECT": true,
	"SESSION_USER": true, "SIMILAR": true, "SOME": true, "SYMMETRIC": true,
	"SYSTEM_USER": true, "TABLE": true, "TABLESAMPLE": true, "THEN": true,
	"TO": true, "TRAILING": true, "TRUE": true, "UNION": true,
	"UNIQUE": true, "USER": true, "USING": true, "VARIADIC": true,
	"VERBOSE": true, "WHEN": true, "WHERE": true, "WINDOW": true,
	"WITH": true,
}
