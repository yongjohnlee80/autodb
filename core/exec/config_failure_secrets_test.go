package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// secretShapes are the ways a credential actually reaches a connection string,
// each with the token that must never appear anywhere outside this process.
//
// TAKEN FROM A MEASUREMENT, NOT FROM IMAGINATION. Before the closed set
// existed, the audit detail was the cause's text, and driving these same DSNs
// through it showed exactly which redaction a driver does and does not do: pgx
// masks the password in a URL's userinfo and nothing else. The host leaked. A
// password passed as a QUERY PARAMETER leaked verbatim. A PAT placed in the
// USERNAME position leaked whole, because the redactor only looks at the
// password field. Every row below reddened then.
var secretShapes = []struct {
	name   string
	dsn    string
	secret string
}{
	{"a plain password in the userinfo", "postgres://autodb_rw:hunter2@db7.internal:66666/billing", "hunter2"},
	{"a percent-encoded password", "postgres://u:p%40ssw0rd%21@db7.internal:66666/d", "p%40ssw0rd%21"},
	{"a password passed as a query parameter", "postgres://u@db7.internal:1/d?sslmode=bogus&password=second_secret", "second_secret"},
	{"a PAT in the username position", "postgres://adb_pat_aaaaaaaaaa.bbbbbbbb@db7.internal:99999/d", "adb_pat_aaaaaaaaaa.bbbbbbbb"},
	{"the target host itself", "postgres://u:p@db7.internal:66666/billing", "db7.internal"},
}

// NOTHING A CONNECTION STRING CARRIES MAY LEAVE THIS PACKAGE.
//
// The two sinks are checked together because they had the same defect and only
// one of them looks dangerous. A log line is understood to be sensitive; an
// audit Detail is not, and it is the wider of the two — Event.Detail reaches
// whatever consumes the event stream, which is more places than an operator's
// terminal.
func TestConfigFailure_NoConnectionStringReachesAuditOrLog(t *testing.T) {
	for _, sh := range secretShapes {
		t.Run(sh.name, func(t *testing.T) {
			verr := ValidateDSN("postgres", sh.dsn)
			if verr == nil {
				t.Skipf("this DSN is accepted, so it produces no failure to inspect")
			}
			// The raise site's own arguments, exactly as conns.go passes them.
			c := NewConfigFailure(ConfigStageDSN, 42, DetailDSNUnusable, verr)

			for _, out := range []struct{ name, value string }{
				{"AuditDetail", c.AuditDetail()},
				{"SafeLog", c.SafeLog()},
				{"Error", c.Error()},
			} {
				if strings.Contains(out.value, sh.secret) {
					t.Errorf("%s carries %q\n  full: %s", out.name, sh.secret, out.value)
				}
				// The DSN's scheme is a cheap proxy for "any of it got out".
				if strings.Contains(out.value, "postgres://") {
					t.Errorf("%s carries a connection string\n  full: %s", out.name, out.value)
				}
			}

			// WHAT MUST REMAIN. A diagnostic that leaks nothing and says
			// nothing is not an improvement: an operator has to be able to
			// find the row and know which check failed.
			d := c.AuditDetail()
			for _, want := range []string{"stage=dsn", "conn=42", string(DetailDSNUnusable)} {
				if !strings.Contains(d, want) {
					t.Errorf("AuditDetail = %q, want it to carry %q", d, want)
				}
			}
			// And the cause is still reachable in-process, for a caller that
			// has decided it is safe to look.
			if c.Cause() == nil {
				t.Error("the cause was discarded; an operator debugging this in a " +
					"debugger or a core dump has nothing left")
			}
		})
	}
}

// EVERY RAISE SITE NAMES A MEMBER OF THE CLOSED SET.
//
// The set only works if nothing constructs a ConfigFailure with a detail
// invented at the call site. A string field cannot enforce that by its type —
// ConfigDetail is a defined string, so a literal conversion compiles — so the
// source is walked instead.
//
// IT WALKS THE AST rather than grepping, because a grep matches this file's own
// prose about the thing it forbids, which is a vacuous check this repository
// has already shipped once.
func TestConfigFailure_EveryRaiseSiteUsesTheClosedSet(t *testing.T) {
	known := map[string]bool{}
	for _, d := range configDetails() {
		known[string(d)] = true
	}
	// The identifier names, which is what a call site actually writes.
	allowedIdents := map[string]bool{
		"DetailUnknownEngine": true, "DetailDSNUnusable": true, "DetailPoolRefused": true,
		"DetailGrammarUnproved": true, "DetailNoDestroy": true, "DetailStoreUnavailable": true,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing core/exec: %v", err)
	}

	sites := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "NewConfigFailure" {
					return true
				}
				sites++
				if len(call.Args) != 4 {
					t.Errorf("%s: NewConfigFailure takes 4 arguments", fset.Position(call.Pos()))
					return true
				}
				arg, ok := call.Args[2].(*ast.Ident)
				if !ok || !allowedIdents[arg.Name] {
					t.Errorf("%s: the detail argument is not a member of the closed set; a "+
						"raise site that composes its own text is a raise site that can "+
						"leak something nobody thought of", fset.Position(call.Args[2].Pos()))
				}
				return true
			})
		}
	}

	// THE VACUITY FLOOR. A walk that found no call sites would pass in silence,
	// and a rename is exactly how this stops watching anything.
	if sites < 5 {
		t.Fatalf("found %d NewConfigFailure call sites in core/exec; the walk is not "+
			"reaching the raise sites and is guarding nothing", sites)
	}
}
