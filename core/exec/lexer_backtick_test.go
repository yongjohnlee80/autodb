package exec

import (
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// A backtick quotes an identifier only where the target says so. PostgreSQL
// reads one as an operator character, so a ';' after it ends the statement
// there; read as a quote, the text hid a second statement from the gate.
func TestBackticks_QuoteOnlyWhereTheTargetQuotes(t *testing.T) {
	const smuggle = "SELECT `a; DROP TABLE t; --`"

	if _, err := classifyFor(smuggle, engine.Postgres); !errors.Is(err, ErrMultiStatement) {
		t.Errorf("postgres: %q classified with err %v; want ErrMultiStatement — the DROP is a statement there", smuggle, err)
	}
	parts, err := splitStatementsFor(smuggle, engine.Postgres)
	if err != nil || len(parts) != 2 || parts[1] != "DROP TABLE t; --`" {
		t.Errorf("postgres: %q split into %q (err %v); want the SELECT, then the DROP with its comment", smuggle, parts, err)
	}

	for _, eng := range []engine.Name{engine.MySQL, engine.SQLite} {
		st, err := classifyFor(smuggle, eng)
		if err != nil || st.Class != ClassRead {
			t.Errorf("%s: %q classified as %v (err %v); want one read, the backticks naming a column", eng, smuggle, st.Class, err)
		}
		parts, err := splitStatementsFor("SELECT `a;b`; SELECT 2", eng)
		if err != nil || len(parts) != 2 || parts[0] != "SELECT `a;b`" {
			t.Errorf("%s: split %q (err %v); want the quoted name kept whole", eng, parts, err)
		}
	}
}
