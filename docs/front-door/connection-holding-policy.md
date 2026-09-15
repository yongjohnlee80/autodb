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

**The debug profile is deprecated and selects nothing.** Every session is now
treated as a debugging session, so the two tiers have collapsed into one bound
that both states receive. The flag and its configuration key are retained to
avoid a schema change; configuration refuses a deprecated value that differs
from the common one, because a key describing behaviour the runtime does not
have is worse than one that is merely ignored.

## Changing the bounds on a running daemon

Every setting in the table above can be replaced without a restart:

```
policy.show    <token>
policy.reload  <token> <session_idle_ms> <idle_in_tx_ms> <max_tx_ms> <max_tx_ceiling_ms> <max_target_conns>
```

`policy.show` needs only a valid token — an operator diagnosing "why did my
transaction end" needs to see the bound that ended it. `policy.reload` is
**admin only**, because these numbers decide how long anyone may hold a
production connection.

Four things about a reload are worth knowing before you use one.

**It replaces all five values at once.** There is no way to change one and
leave the others, and that is deliberate: a partial update needs a way to say
"leave this alone", and the difference between *unset* and *zero* is exactly
where a safety bound goes missing. Read the current values with `policy.show`,
change the ones you mean to, and submit the whole set.

**It is validated before anything moves.** The rules are the same ones the
configuration file is held to at startup, so a reload cannot put the daemon
into a state it would refuse to boot into. A rejected reload changes nothing —
not the live bounds, not the stored policy, and no audit row is written.

**It survives a restart.** The new policy is written to the meta store in the
same transaction as its audit record, and the daemon applies it at startup
before it serves anything. A change made at 3am is still in force after the
next deploy. If a stored policy has stopped being valid — because the pool's
idle time or the janitor's interval moved underneath it — the daemon **refuses
to start** rather than quietly running bounds nobody chose.

**Lowering the budget does not close anything.** See *Lowering the budget
drains* below; the same is true here.

The audit row is `policy_reloaded` and records the generation, every value
before and after, and the user who made the change.

## Every idle transaction announces itself

An open transaction that has been idle for thirty minutes writes a high-severity
`idle_in_transaction_holder` audit record, and another every thirty minutes
after that until it either does something or is reclaimed.

This is what makes a two-hour idle bound safe to have. The bound is generous
because a developer debugging needs to think rather than race a clock — but
generous bounds are only safe if somebody can see what is holding a lock while
it is still holding it. Previously a holder was invisible until the moment it
was reclaimed, so the trail recorded the ending and never the two hours of
waiting that led there.

Each record carries who is holding it, from where, since when, which token
identifies them, how many other backends that same account is holding, how many
statements the session has run, and a bounded preview of the last one. It
carries no token, no password and no bind values. Fields that are genuinely
unknown — the backend PID, which nothing in the pinned-connection seam exposes
— say so explicitly rather than reporting a zero that reads like an answer.

**It is a record, not a page.** The heartbeat never reclaims anything and never
cancels anything; whether it wakes somebody is a routing decision made
elsewhere. At the two-hour bound the reclamation record is what fires, and the
heartbeat that would otherwise coincide with it is suppressed — so an episode
reads 30m, 60m, 90m, then the ending, in that order, every time.

## The connection budget

`exec.max_target_conns` is this instance's total production-connection budget:
the number of sockets it may have open to target databases at once, across
every target. Call it **C**. **C must be at least 2**, for the reason below.

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

**It is not the per-source connection cap.** That is a different resource,
limiting concurrent connections from one address, and the two must not move
together.

### One slot is reserved for cancellation

PostgreSQL does not cancel a statement on the session that issued it: it
requires a **second connection** carrying the backend key. That second socket
is dialled through the same path as ordinary work — so if ordinary work may
take the whole budget, a cancellation is refused exactly when someone reaches
for it, because a developer cancels a query when the system is busy, not when
it is idle.

So the last slot is reserved:

- **ordinary limit = C − 1.** Ordinary dials stop one short.
- **control limit = 1.** One holder at a time, serialized. A cancel is
  connect, write sixteen bytes, close; one lane is enough, and more would be a
  second budget nobody configured.
- The control dial is **marked** before it is made, and only code inside the
  engine can mark it, so ordinary traffic cannot help itself to the reserved
  slot.
- It is **exact-backend cancellation only** — that one statement, on that one
  connection.

**C ≥ 2** follows: at C = 1 the reserved slot consumes the whole budget and
nothing is left for work.

### The accounting

Two occupancies are tracked separately:

| Symbol | Meaning |
| --- | --- |
| **O** | ordinary sockets live or in flight |
| **K** | control occupancy, 0 or 1 |

| Quantity | Definition |
| --- | --- |
| `Outstanding` | **O + K** — every socket this instance holds |
| `Effective` | **max(C, O + 1)** — the immediately exercisable ceiling |
| `Draining` | **O > C − 1** |

**`Effective` uses O + 1, not Outstanding**, and the +1 applies whether or not
a cancel is in flight. The reserved slot is part of the ceiling at all times,
because a cancel may arrive at any moment. Defining it against `Outstanding`
instead makes the number rise and fall as short-lived cancel traffic comes and
goes — within a single drain generation it would read 49, then 50, then 49,
and an operator would see a drain apparently reverse.

**The cancellation is counted, never exempt.** It occupies a slot and appears
in `Outstanding`. What it must not do is move the ceiling.

### Lowering the budget drains; it does not kill

With 50 sockets open and a new budget of 25, nothing is closed. Ordinary dials
stop; the number becomes true by attrition. Closing live sessions to make a
configured number true would turn a configuration edit into an outage.
Monotonicity is therefore asserted per generation rather than absolutely, and
`Effective` converges **downwards** to C as ordinary sockets retire — it never
rises within a generation.

**The control lane is the one exception, and it is deliberate.** While
draining, O is above the ordinary limit C − 1 by definition — O = C is already
draining — so a control lane that re-tested
against the budget would refuse a cancellation precisely because too much work
is already running, which is the moment cancelling matters most. Ordinary and
unmarked dials stop; the marked control dial may still proceed.

Worked, for C dropping 50 → 25 with 49 ordinary sockets held:

| State | O | K | Outstanding | Effective |
| --- | ---: | ---: | ---: | ---: |
| idle lane | 49 | 0 | 49 | 50 |
| cancel in flight | 49 | **1** | **50** | 50 |
| cancel released | 49 | 0 | 49 | 50 |

`Effective` is 50 throughout: `max(25, 49 + 1)`. It does not move when the
lane fills and empties, and it falls towards 25 only as the 49 ordinary
sockets retire.

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
