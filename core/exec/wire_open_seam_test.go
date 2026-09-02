package exec

import "testing"

// The lease rule's predicate: UTF8 in either spelling passes; anything else,
// SQL_ASCII included, is refused with the offending parameter named.
func TestLeaseEncodingRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		st     map[string]string
		refuse bool
		want   string
	}{
		{"utf8", map[string]string{"server_encoding": "UTF8", "client_encoding": "UTF8"}, false, ""},
		{"utf-8 spelling", map[string]string{"server_encoding": "UTF-8", "client_encoding": "utf8"}, false, ""},
		{"latin1 server", map[string]string{"server_encoding": "LATIN1", "client_encoding": "UTF8"}, true, "server_encoding=LATIN1"},
		{"sql_ascii", map[string]string{"server_encoding": "SQL_ASCII", "client_encoding": "UTF8"}, true, "server_encoding=SQL_ASCII"},
		{"client not utf8", map[string]string{"server_encoding": "UTF8", "client_encoding": "WIN1252"}, true, "client_encoding=WIN1252"},
		{"empty set (fail closed)", map[string]string{}, true, "server_encoding missing"},
		{"nil set (fail closed)", nil, true, "no reported statuses"},
		{"client key missing (fail closed)", map[string]string{"server_encoding": "UTF8"}, true, "client_encoding missing"},
	} {
		got, refused := leaseEncodingRefusal(tc.st)
		if refused != tc.refuse || got != tc.want {
			t.Fatalf("[%s] refused=%v got %q; want %v %q", tc.name, refused, got, tc.refuse, tc.want)
		}
	}
}
