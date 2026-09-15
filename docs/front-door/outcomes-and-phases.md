# Outcomes and phases

What can happen to a connection, who is allowed to say it happened, and what it
costs the peer.

---

## The problem this solves

Three vocabularies existed and none of them was a registry. Statement admission
had its codes, the front door had its private denial reasons, and the engine
had its own strings. Each was complete by somebody's care rather than by
construction, and nothing connected them.

Three consequences followed.

**A reason nobody classified was charged by default.** That is how a developer
who ran out of connections was banned for grinding credentials: capacity
refusals fell through to the charging branch because no one had said they
shouldn't.

**Some outcomes belonged to no vocabulary at all.** A cancel request is not a
denial — nothing is refused, a request is handled and the connection closes. A
TLS handshake that fails, a startup code nobody recognises, a peer that hangs up
before asking anything: none of these is a refusal either. All of them lived as
bare string literals at the call site, where nothing could enumerate them,
classify them, or notice one going missing.

**One string meant two things.** A startup parameter could be refused before any
credential (charged as protocol grinding) or after a verified token (not charged
at all), and both audited under the same name. See the note in the protocol
matrix under startup parameters.

## The registry

`core/outcome` holds identity, and identity alone.

An **identity** is a `ReasonID`. The three existing types adapt to it by
conversion — no rename, no value change, no table. A **producer** is the phase
or stage that declares it. **Membership belongs to the producer, not to the
reason**, because one identity genuinely has several: the engine *raises* a
capacity refusal and the front door *renders* it, and a field on the reason
forces one of them to lie.

Every declaration states a **kind** (refusal, control, operational, note) and a
**charge**. Both zero values are rejected at composition, because the failure
mode of a forgotten charge is that something gets charged by accident.

| Charge | Counts against the source? | Meaning |
| --- | :---: | --- |
| `Credential` | yes | the peer presented something wrong |
| `Protocol` | yes | the peer spoke the protocol wrongly, or failed a handshake |
| `Capacity` | **no** | *we* ran out |
| `None` | **no** | our configuration, our stored state, our bug |
| `NotApplicable` | **no** | there is no per-source counter within reach |

`NotApplicable` is not `None`. A statement-level decision happens on an
authenticated session already past every accept-time budget; saying "none" would
claim somebody decided not to charge it, when the question does not arise.

**Composition rejects** a duplicate declaration by one producer, an identity two
producers describe differently, and anything with a missing field. The daemon
composes at start-up and does not start if its producers disagree — the
alternative is discovering it at the moment it fires, in whichever direction the
last registration happened to win.

### The registry never renders

The obvious design is a table from reason to wire code, and it breaks on the
first row. A capacity refusal is `28000` to a stranger and `53300` to a caller
who has already proved who they are; a table has to pick one and is wrong for
the other. Accept-phase outcomes have no frame at all.

So the thing that varies travels with the **event**, not the identity. An
`Occurrence` carries the authorization witness, and the renderer projects from
it — never from a reason-only lookup. The witness is set at the raise site and
carried; deriving it from the reason is what would leak the moment somebody
reordered a check.

## The phases

The phases are the ones the code has, not the ones a tidy diagram would want.

| Phase | Wraps | Cancellation today | Re-runnable? |
| --- | --- | --- | :---: |
| `accept` | reserve-or-refuse in the accept loop | socket deadline | no |
| `startup` | TLS negotiation and the startup packet | socket deadline | no |
| `cancel` | the cancel-request branch | context | no |
| `authenticate-and-open` | the engine's single atomic call | context | no |
| `handshake` | the transition into the session | socket deadline | no |
| `serve` | the session loop and its teardown | **detached** | no |

Credential verification and session open are **one phase** because they are one
call: it verifies the token, takes the reservation, pins the backend, applies
settings and publishes the session without returning in between. Naming them
separately would describe a structure the code does not have, and giving them a
real boundary needs its own token, lock and rollback design.

Cancellation modes are **recorded, not imposed**. The phases genuinely use a
mixture, and `serve`'s teardown runs with cancellation dropped on purpose so a
shutdown does not abandon the cleanup it caused. Forcing one model would be a
behaviour change wearing a refactor's clothes.

Every phase is exactly-once and **refuses a rerun rather than performing one**.
Each takes an atomic reservation, consumes bytes off a socket, or publishes a
session; nothing should promise a repeat the code cannot deliver.

## What a phase may conclude

Four outcomes, and **there is no wait**:

```
Continue()      Refuse(id, …)      Operational(err)      TerminalControl(id, …)
```

The discriminant is unexported and these are the only constructors, so the
scheduler work that eventually needs a peer to wait for capacity must *extend*
this API — a compile-visible change its reviewer meets on the first line of the
diff, rather than a state that was quietly reachable all along.

**The zero value fails closed.** A phase that returned it fell off the end of
its own logic, and the safe reading of "I did not say" is never "carry on".

**A fault is not a success.** If a phase cannot run — an identity nobody
declared, a rerun of something exactly-once — the connection ends. The first
version of the runner logged it and carried on, which left the startup result at
its zero value and the code below read that as "startup completed, no denial": a
connection would proceed to the credential exchange having negotiated nothing.
On the credential path the peer still gets the ordinary uniform denial, because
silence there would turn a one-line declaration mistake into a client that hangs
on a read.

## The accept token

Accept hands on three obligations as one value: the socket, the handler
registration, and the accept-time reservation.

The handler registration is the one that matters. The accept loop adds to the
wait group *before* admission and before the goroutine exists — that ordering is
what stops `Close` observing the counter at zero while a connection is still
arriving — and the obligation is then transferred into the handler. Exactly one
consumer takes the token: the refusal path or the handler.

A token dropped without discharge is a `Close` that hangs forever. A token
discharged twice is a counter that goes negative, which panics the process
during shutdown. Neither has a symptom until somebody restarts the daemon under
load, so the invariant is carried by the type rather than by everyone
remembering.

**Release is LIFO and it is not decoration.** The tracking entry goes before the
socket closes, or a concurrent `Close` walks the live set and closes a connection
this goroutine is already closing. The close event is announced before the
reservation is returned, because emitting it afterwards would let `Close` return
— and the process exit — with the last event of a connection's life unwritten.
The reservation goes before the handler registration, or `Close` can observe zero
while an accept-time reservation is still held.

## How this is checked

`frontdoor/testdata/lifecycle-baseline.txt` is the no-op proof: the whole
refactor above changed no byte of it. See `lifecycle-baseline.md`.

Beside it, the structural cells: no wait is constructible (read out of the
source, so the claim cannot quietly stop being true), the zero value fails
closed, an exactly-once phase refuses a rerun *without running the body*, an
undeclared identity is refused, the witness reaches an occurrence only when
given, and every reason a phase raises is declared by that phase's producer —
checked at build time by reading the raise sites, because discovering a
declaration mistake on a live denial path is the wrong way to discover it.
