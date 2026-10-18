package autodocplugin

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// TLS FILES IN A DSN — the Add connection dialog's TLS files written into the DSN in each engine's
// own DSN syntax, as the AutoDB server's parser reads it (core/exec/dsn.go).

// tlsDSN is dsn with the TLS files written in as the server reads them: PostgreSQL's sslrootcert,
// sslcert and sslkey, verify-full when a CA is given and the DSN names no sslmode; MySQL's tls-ca,
// tls-cert and tls-key, which the AutoDB server makes a TLS config of. SQLite has none. The paths
// are the server's: a remote server reads its own files.
func tlsDSN(eng, dsn, ca, cert, key string) (string, error) {
	if ca == "" && cert == "" && key == "" {
		return dsn, nil
	}
	if (cert == "") != (key == "") {
		return "", errors.New("a TLS certificate and its key go together")
	}
	switch engine.Name(eng) {
	case engine.Postgres:
		params := [][2]string{{"sslrootcert", ca}, {"sslcert", cert}, {"sslkey", key}}
		if ca != "" && !strings.Contains(dsn, "sslmode") {
			params = append(params, [2]string{"sslmode", "verify-full"})
		}
		return withParams(dsn, params, pgKeyword), nil
	case engine.MySQL:
		return withParams(dsn, [][2]string{{"tls-ca", ca}, {"tls-cert", cert}, {"tls-key", key}}, nil), nil
	}
	return "", fmt.Errorf("%s has no TLS files", eng)
}

// withParams adds the params with a value to dsn: as a URL's query (a postgres:// URL, or MySQL's
// DSN, whose query is URL-escaped), or, for a PostgreSQL keyword DSN, as keyword pairs.
func withParams(dsn string, params [][2]string, keyword func(string) bool) string {
	if keyword != nil && keyword(dsn) {
		var b strings.Builder
		b.WriteString(dsn)
		for _, kv := range params {
			if kv[1] != "" {
				b.WriteString(" " + kv[0] + "='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(kv[1]) + "'")
			}
		}
		return b.String()
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	var b strings.Builder
	b.WriteString(dsn)
	for _, kv := range params {
		if kv[1] != "" {
			b.WriteString(sep + kv[0] + "=" + url.QueryEscape(kv[1]))
			sep = "&"
		}
	}
	return b.String()
}

// pgKeyword reports whether dsn is a PostgreSQL keyword DSN ("host=… dbname=…"), not a URL.
func pgKeyword(dsn string) bool {
	return !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://")
}
