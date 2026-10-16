package engine

import gsql "github.com/yongjohnlee80/golib/parse/sql"

// StatementSplitter is golib's SQL splitter configured with this engine's
// lexical rules — the same capabilities the statement gate's lexer reads — so
// a statement an editor finds with it begins where the gate begins one.
//
// It is built from the table, not from a golib preset, because the two must
// agree rule for rule: the gate reads no trigger bodies (it splits a CREATE
// TRIGGER at the semicolons inside it, refusing more rather than less), so the
// splitter reads none either, where golib's SQLite preset would.
func (n Name) StatementSplitter() gsql.SQL {
	var opts []gsql.Option
	if n.BackslashEscapes() {
		// MySQL's rules ride on one capability, as the gate's lexer reads them:
		// backslash escapes, # comments, -- only before a space, and /*! … */
		// whose body is live.
		opts = append(opts, gsql.BackslashEscapes(), gsql.HashComments(),
			gsql.MySQLDashComments(), gsql.ExecutableComments())
	}
	if n.DollarQuotedStrings() {
		opts = append(opts, gsql.DollarQuotes())
	}
	if n.NestedBlockComments() {
		opts = append(opts, gsql.NestedBlockComments())
	}
	if n.EscapeStringConstants() {
		opts = append(opts, gsql.EStringEscapes())
	}
	if n.BacktickQuotedIdentifiers() {
		opts = append(opts, gsql.Backticks())
	}
	if n.BracketQuotedIdentifiers() {
		opts = append(opts, gsql.BracketIdentifiers())
	}
	if n.TclVariables() {
		opts = append(opts, gsql.TclParameters())
	}
	return gsql.New(opts...)
}
