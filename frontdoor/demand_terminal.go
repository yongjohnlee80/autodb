package frontdoor

import (
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// ENDING A SESSION IS NOT REFUSING A REQUEST, AND IT DOES NOT BELONG IN THE
// REGISTER OF REFUSALS.
//
// This row lived in the held-object register for a while, and it did not fit
// twice over. That register is for conditions a session's own prepared
// statements and portals produce — every row is a client asking for something
// and being told no, every row is raised by an engine sentinel, and the
// register's cells assert exactly that uniformity. Demand reclamation has
// neither property: nobody asked for anything, and nothing raises it but a
// scheduler deciding that an idle session's connection should go to somebody
// who has been waiting.
//
// IT WAS PUT THERE BECAUSE THE RENDERING WAS CONVENIENT. frameHeldObject
// already knew how to send a fatal frame, so the row went where the mechanism
// was, and inherited a contract about what it MEANT. Keeping it there would
// have forced one of two bad choices: call a session we deliberately ended a
// "refusal", which makes an operator counting refusals count terminations too;
// or loosen an invariant that holds for every genuine member of that table.
//
// So the mechanism is shared and the ownership is not. This is its own
// producer, its own row, and its own declaration.

// ProducerDemandReclamation owns the one outcome a demand reclamation can
// produce. Separate from the held-object producer because the two answer
// different questions about what happened.
const ProducerDemandReclamation = outcome.ProducerID("demand-reclamation")

// OutcomeDemandReclaimed is a session ended so that its server connection could
// serve a request that was waiting for one.
//
// CONTROL, NOT REFUSAL. Nobody was refused: a session that was working
// perfectly well was ended, by us, for somebody else's benefit. Filing it as a
// refusal would put it beside "we would not do that for you", and the count an
// operator reads for refusals would silently include sessions we chose to
// terminate — a different number, with a different remedy.
const OutcomeDemandReclaimed = "frontdoor/demand-reclaimed"

// demandTerminalRow is the frame a reclaimed session's client receives.
//
// THE MESSAGE SAYS ONLY WHAT IS TRUE OF EVERY SESSION IT ENDS. An earlier
// version reused the reserved no-mechanism row, which tells the client it held
// prepared statements or portals; selection never required those, so most
// clients would have been told something false about their own session in the
// one message whose whole job is to explain what happened.
func demandTerminalRow() terminalFrame {
	return terminalFrame{
		identity: OutcomeDemandReclaimed,
		sqlState: sqlStateAdminShutdown,
		severity: "FATAL",
		message: "this session's server connection was reclaimed while the session was " +
			"idle, so that a connection request that was waiting for one could be served",
		hint: "reconnect; a session that is left idle may have its server connection " +
			"reclaimed when others are waiting for one",
	}
}

// demandTerminalDecls is what this producer can emit: exactly one outcome.
//
// NotApplicable for the same reason as its neighbours in other tables: the
// session is long past every accept-time budget, so no per-source counter is in
// reach, and "we decided not to charge it" would claim a decision nobody had
// the opportunity to make.
func demandTerminalDecls() []outcome.Decl {
	return []outcome.Decl{{
		ID:     outcome.ReasonID(OutcomeDemandReclaimed),
		Kind:   outcome.Control,
		Charge: outcome.NotApplicable,
	}}
}

// terminalFrame is the shape a fatal frame is rendered from.
//
// SHARED MECHANICS, SEPARATE OWNERSHIP. The held-object rows carry the same
// five fields plus their own object-specific columns — what to do with the
// segment, what the transaction becomes — none of which mean anything here.
// Reusing the renderer is sensible; reusing the register is what put a
// termination in a table of refusals.
type terminalFrame struct {
	identity string
	sqlState string
	severity string
	message  string
	hint     string
}
