# Connection-holding policy

How long a session may hold a production connection, how much of a database
server this instance may claim (its connection pools), and what happens when
it runs out.

This file exists so the code can point at something a reader can open. The
design record lives in the team knowledge base, which a reader of this
repository has no access to, so the rules that govern the code are restated
here in full.

---

## The bounds

| Setting | Value | What it governs |
| --- | ---: | --- |
| `exec.session_idle_timeout` | **10 min** | An idle session with no open transaction |
| `exec.idle_in_tx_timeout` | **30 min** | Idle *inside* an open transaction |
| `exec.max_tx_duration` | **2 h** | Total wall-clock life of one transaction |
| `exec.max_tx_duration_ceiling` | **2 h** | Hard cap on the line above |

Socket budgets are not in this table: they are per connection pool, in the
meta store (*Connection pools* below).

**No timeout exceeds two hours** (Johno, 2026-11-07: no transaction should run
longer than 2 hours). A policy reload above 2 h is refused, naming the setting.
A config file or a stored policy above it still starts the daemon: the value is
clamped to 2 h with a warning, and a clamped stored policy is re-written with a
`policy_clamped` audit row. Every timeout's value lives in one Go package,
`core/timeouts`.

**The two transaction bounds do different jobs.** The idle bound catches the
*abandoned* transaction — someone opens one and walks away, and it is gone in
thirty minutes whatever the total says, and the pool capacity it held is free
again. The duration bound caps the *long but attended* one at two hours.

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
policy.reload  <token> <session_idle_ms> <idle_in_tx_ms> <max_tx_ms> <max_tx_ceiling_ms>
```

A client still sending a fifth value, `max_target_conns`, is refused with a
message naming `pool.update`, which replaced it.

`policy.show` needs only a valid token — an operator diagnosing "why did my
transaction end" needs to see the bound that ended it. `policy.reload` is
**admin only**, because these numbers decide how long anyone may hold a
production connection.

Four things about a reload are worth knowing before you use one.

**It replaces all four values at once.** There is no way to change one and
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

**It touches no socket budget.** Those are per pool, changed with
`pool.update`.

The audit row is `policy_reloaded` and records the generation, every value
before and after, and the user who made the change.

## Every idle transaction announces itself

An open transaction that has been idle for thirty minutes writes a high-severity
`idle_in_transaction_holder` audit record, and another every thirty minutes
after that until it either does something or is reclaimed.

This is what makes a long idle bound safe to have. The bound is generous
because a developer debugging needs to think rather than race a clock — but
generous bounds are only safe if somebody can see what is holding a lock while
it is still holding it. Previously a holder was invisible until the moment it
was reclaimed, so the trail recorded the ending and never the wait that led
there.

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

## Connection pools

A **connection pool** (`db_pool_connections`) is the socket budget a group of
connections shares: the sockets this instance holds to all of a pool's
connections together never exceed the pool's `max_conns`. An admin creates
pools to match database servers; autodb never assumes two connections share a
server. Every connection starts in **`default`** (25), and an admin moves it.
MySQL and SQLite connections are outside every pool and keep
`exec.pool_max_conns` as their own cap.

```
pool.list      <token>
pool.create    <token> <name> <max_conns>         admin
pool.update    <token> <pool_id> <max_conns>      admin
pool.delete    <token> <pool_id>                  admin
conn.set_pool  <token> <conn_id> <pool_id>        admin
```

`max_conns` is from 3 to 10000. **Choose at most what the server's
`max_connections` leaves for this instance**: autodb cannot see that number,
and summing across instances is the operator's job. The default pool and any
pool a connection (archived ones included) belongs to cannot be deleted.

**It is a runtime permit, not a sum of configured caps.** A permit is taken
when a socket is about to be dialled and released when it closes, so the
budget is spent by connections that actually exist, on whichever of the pool's
databases needs them.

### The split

A pool of **C** sockets is three parts:

| Part | Size | Who uses it |
| --- | ---: | --- |
| front-door share | C − 1 − headroom | front-door sessions' backends |
| headroom share | min(C / 2, 4) | the TUI, exec and autodb's own queries |
| control lane | 1 | cancellations |

A pool of 25 is 20, 4 and 1. Each share is bounded by itself, so a burst of
exec work can't take the sockets front-door sessions were admitted against,
and a full front door can't starve autodb's own queries.

The session line at the front door admits as many front-door sessions as the
front-door share, so an admitted session's backend always has its socket. A
dial whose share is full **waits in that share's line**, first come first
served, for as long as its caller does; a caller with no deadline waits up to
the queue wait (90 s), then is refused as a capacity matter (`53300`).

**No backend in a pool is reused.** Every release closes the socket, so its
permit returns at once and no idle connection holds a pool's capacity. A
connection whose caller gave up while it was being dialled is reaped in about
a second.

### One slot is reserved for cancellation

PostgreSQL does not cancel a statement on the session that issued it: it
requires a **second connection** carrying the backend key. If the shares could
take the whole budget, a cancellation would be refused exactly when someone
reaches for it. So the last slot is the control lane's: one holder at a time,
serialized, reachable only by a dial the engine marks, and for exact-backend
cancellation only. It waits for the lane on its own deadline rather than being
lost.

### The accounting

| Symbol | Meaning |
| --- | --- |
| **F** | front-door sockets live or in flight |
| **O** | headroom (ordinary) sockets live or in flight |
| **K** | control occupancy, 0 or 1 |

| Quantity | Definition |
| --- | --- |
| `Outstanding` | **F + O + K** — every socket the pool holds |
| `Effective` | **max(C, F + O + 1)** — the immediately exercisable ceiling |
| `Draining` | a share above its current limit |

**`Effective` counts the lane whether or not a cancel is in flight**, so the
number can't rise and fall with short-lived cancel traffic inside one drain.

### Lowering a pool drains; it does not kill

With 49 sockets open and a pool lowered from 50 to 25, nothing is closed. New
dials above the new shares wait; the number becomes true by attrition.
Monotonicity is asserted per generation. The control lane is the one
exception: it is never re-tested against the budget, because a drain is when
cancelling matters most. Raising a pool serves both shares' lines at once, and
no target's connection pool is rebuilt.

### Moving a connection

`conn.set_pool` ends every session on the connection first, as a profile
downgrade does, because a live socket can't change pools. Anyone connecting
meanwhile is told the connection is being changed and to retry. A socket the
bounded close outlives stays charged to the old pool, which granted it, until
it closes.

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
