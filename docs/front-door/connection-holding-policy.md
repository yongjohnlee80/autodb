# Connection-holding policy

How long a session may hold a production connection, how much of a target
database this instance may claim, and what happens when it runs out.

This file exists so the code can point at something a reader can open. The
design record lives in the team knowledge base, which a reader of this
repository has no access to, so the rules that govern the code are restated
here in full.

---

## The bounds

| Setting | Value | What it governs |
| --- | ---: | --- |
| `exec.session_idle_timeout` | **10 min** | An idle session with no open transaction |
| `exec.idle_in_tx_timeout` | **2 h** | Idle *inside* an open transaction |
| `exec.max_tx_duration` | **8 h** | Total wall-clock life of one transaction |
| `exec.max_tx_duration_ceiling` | **8 h** | Hard cap on the line above |
| `exec.max_target_conns` | **no default** | Total sockets to targets, across every target |

**The two transaction bounds do different jobs.** The idle bound catches the
*abandoned* transaction — someone opens one and walks away, and it is gone in
two hours whatever the total says. The duration bound protects the *long but
attended* one. So a forgotten transaction costs two hours rather than eight,
and nothing survives overnight.

**The session bound is short on purpose.** An idle session holding no
transaction is the only thing that can be reclaimed freely, so a long bound
there starves everyone else. Ten minutes also keeps it at or under
`pool_max_conn_idle_time`, so an idle session never holds a backend checked
out past the point the pool would have closed it.

**The ceiling must move with `max_tx_duration`.** Left lower it silently clips
it — no error, no log line, just transactions dying at a bound no document
describes.

**The debug profile is deprecated.** Every session is now treated as a
debugging session, so the two tiers have collapsed. The flag is retained to
avoid a schema change and may only ever *lengthen* a bound, never shorten it.

## The connection budget

`exec.max_target_conns` is this instance's total production-connection budget:
the number of sockets it may have open to target databases at once, across
every target.

**It has no default and is required when the front door is enabled.** A
default would have this process quietly claim a number nobody chose, against a
database whose `max_connections` it cannot see. Choose at most half the target
server's `max_connections`; that is guidance, not validation, and summing
across several instances is the operator's job.

**It is a runtime permit, not a sum of configured caps.** Summing each
target's `pool_max_conns` pre-slices the budget: two targets configured at 8
reserve 16 whether or not either is busy, so one queues while the other's
slice sits idle and nothing can rebalance. A permit is taken when a socket is
about to be dialled and released when it closes, so the budget is spent by
connections that actually exist.

**Per-target `pool_max_conns` remains a technical ceiling on one pool.** It is
not an allocation, and two targets may each be allowed more than the budget —
the ledger is what makes that safe.

**Lowering the budget drains; it does not kill.** With 50 sockets open and a
new budget of 25, no new permit is granted above the new budget and the number
becomes true by attrition. Closing live sessions to make a number true would
turn a configuration edit into an outage. Monotonicity is therefore asserted
per generation rather than absolutely.

**It is not the per-source connection cap.** That is a different resource,
limiting concurrent connections from one address, and the two must not move
together.

## Charge classes

A denial says who it is *about*, and that decides whether it counts against
the credential throttle.

| Class | Meaning | Charges? |
| --- | --- | --- |
| `Credential` | the caller presented something wrong | **yes** |
| `Protocol` | the caller's frames were malformed or disallowed | **yes** |
| `Capacity` | we ran out of something | **no** |
| `None` | our own store, config or bug | **no** |

Counting "we are full" as "you guessed wrong" bans the developer who was just
refused for capacity. That is not hypothetical: a pool granting four leases
filed the fifth connection as a failed login, and the tenth banned the source
address.

**Capacity and None never charge.** Reasons that follow a *verified* token
bound to the exact connection — no such database, no grant, unscoped token —
are `None`: the caller has already proved who they are, and what they met is a
fact about what we stored. Charging them means a developer whose grant was
never set up bans themselves by retrying.

**Credential and protocol strictness is unchanged.**

## Panics

A panic in one connection's goroutine must not end every other session. It is
contained at two seams: per admission stage, which is what lets the report name
which stage broke, and per connection as defence in depth for everything
outside a stage.

**A recovered panic is an operational error, never a denial.** Projecting one
as a denial would publish a refusal reason no stage declared, charge the caller
for our bug, and — because the wire renders denials uniformly — make a crash
indistinguishable from a real policy decision. The connection is always closed
and never resumed: post-panic state is precisely the state nobody reasoned
about.
