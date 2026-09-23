package rpc

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/auth"
)

// scrubSecrets has two obligations that pull against each other: remove the
// credential, and keep the diagnosis. Every case below asserts BOTH, because a
// scrubber judged only on what it removes passes by returning "".
func TestScrubSecrets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		gone    []string // must not survive
		kept    []string // must survive: this is what the operator acts on
		exactly string   // when set, the full expected output
	}{
		{
			name:    "url userinfo password",
			in:      "failed to connect to `postgres://autodb_rw:s3cr3tP@db7.internal:6432/billing`: connection refused",
			gone:    []string{"s3cr3tP"},
			kept:    []string{"autodb_rw", "db7.internal", "6432", "billing", "connection refused"},
			exactly: "failed to connect to `postgres://autodb_rw:***@db7.internal:6432/billing`: connection refused",
		},
		{
			name: "url query sslpassword keeps later params",
			in:   "dsn `postgres://u:pw@h:5432/d?sslmode=require&sslpassword=hunter2&connect_timeout=5` unusable",
			gone: []string{"hunter2", "pw@"},
			kept: []string{"sslmode=require", "connect_timeout=5", "h:5432"},
		},
		{
			name: "keyword dsn password keeps later settings",
			in:   "failed to connect to `user=autodb_rw password=hunter2 host=db7.internal database=billing`: timeout",
			gone: []string{"hunter2"},
			kept: []string{"user=autodb_rw", "host=db7.internal", "database=billing", "timeout"},
		},
		{
			name: "a PAT used as a credential keeps its prefix",
			in:   "auth failed for " + auth.PATPrefix + "AbCdEfGhIjKl.v-ERY_s3cretBYTES: 28P01",
			gone: []string{"v-ERY_s3cretBYTES", "AbCdEfGhIjKl"},
			kept: []string{auth.PATPrefix, "28P01"},
		},
		{
			name:    "no secret is left untouched",
			in:      "failed to connect to `user=postgres database=tagus`: 34.118.163.29:5432: dial error: timeout: context deadline exceeded",
			gone:    nil,
			kept:    []string{"34.118.163.29", "5432", "context deadline exceeded", "database=tagus"},
			exactly: "failed to connect to `user=postgres database=tagus`: 34.118.163.29:5432: dial error: timeout: context deadline exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scrubSecrets(tc.in)
			for _, bad := range tc.gone {
				if strings.Contains(got, bad) {
					t.Errorf("secret %q survived scrubbing:\n  %s", bad, got)
				}
			}
			for _, want := range tc.kept {
				if !strings.Contains(got, want) {
					t.Errorf("diagnosis lost %q:\n  %s", want, got)
				}
			}
			if tc.exactly != "" && got != tc.exactly {
				t.Errorf("got  %s\nwant %s", got, tc.exactly)
			}
		})
	}
}

// The scrubber must be idempotent: wireErr applies it once, but a cause that
// has already been through it (a wrapped/re-reported error) must not degrade
// into "***" creeping over the surrounding text.
func TestScrubSecrets_Idempotent(t *testing.T) {
	t.Parallel()
	in := "failed to connect to `postgres://u:pw@h:5432/d?sslpassword=x`: refused"
	once := scrubSecrets(in)
	if twice := scrubSecrets(once); twice != once {
		t.Errorf("not idempotent:\n once: %s\ntwice: %s", once, twice)
	}
}
