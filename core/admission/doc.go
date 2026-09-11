// Package admission is the admission pipeline's leaf vocabulary: the Facts a
// statement carries, the Context it runs in, the Stage interface every guard
// and the future analyzer implement, the Contribution a stage may make, and
// the Orchestrator that composes stages into one ordered, printable chain.
//
// THE CORE DOES NOT KNOW WHAT THE DENY LOGIC IS. The engine asks, receives
// a Report, and if the Report denies, it does not dispatch. Everything that
// decides what a statement may do lives behind the Stage interface — today
// the legacy guards adapted by the engine's own closures, later the
// lexer/AST risk analyzer as a second implementation of the same interface.
// The reciprocal holds by construction: nothing in this package knows the
// wire. No protocol vocabulary, no error-code-to-SQLSTATE mapping, no
// transport types — the front door renders a Reason into ITS vocabulary,
// the RPC into its own, and this package stays reusable by consumers that
// have neither (the schema-aware completion source among them).
//
// A LEAF, ENFORCED BY WHAT IT DOES NOT IMPORT: this package imports nothing
// from core/exec, frontdoor, or any protocol library, and the engine-side
// test suite asserts that boundary rather than trusting it. The types here
// are deliberately admission-local: Facts exposes ACCESSORS (Verb(),
// Mutations(), Calls()…) rather than the engine's statement structs, so the
// engine implements this package's interfaces from outside and the
// dependency arrow points one way only.
//
// Data over interfaces where data is the point: Report is a struct, and
// arms are added by adding fields — no implementor breaks. Contribution
// carries policy (a deny Reason, a risk Observation) and is never an
// error; an error is never a Contribution. The two returns from Apply
// answer different questions — "what is wrong with this statement" and
// "did the stage itself break" — and conflating them is the failure the
// split exists to prevent.
package admission
