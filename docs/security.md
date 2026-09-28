# Security model

Every frontend is a client of one core. There is no path that skips the gates,
because the gates are not in the frontends.

**Identity and authorization.** Three roles ordered `reader < editor < admin`.
Statements are classified — read / write / DDL / control — and a class is
checked against the caller's role *and* their grant on that specific
connection. Connection-scoped actions require a grant **for admins too**: a
globally-`reader` user never exceeds `SELECT`, whatever grants they hold.

**Read-only means read-only.** `reader` users don't merely get their `UPDATE`s
rejected by autodb. Through the front door they run inside
**server-enforced read-only transactions**, so a write smuggled through a
function, a procedure or dynamic SQL fails at PostgreSQL itself with SQLSTATE
`25006`. The database enforces the boundary, not just the proxy in front of it.

**Dangerous-statement detection — deterministic, out of the box.** A
hand-written lexer — not a regex, and not a full parser — decides what a
statement *is* before it runs. This layer needs no configuration, no network
and no model; it is on from the first launch:

- `UPDATE` / `DELETE` with no top-level `WHERE` clause is **blocked**.
- **One statement per call on the single-statement path** — anything after a
  top-level `;` is refused. The script runner deliberately *does* accept a
  multi-statement buffer, but it splits the buffer and puts each statement
  through the same classify → authorize → guard → audit path on its own, so
  the audit record still equals exactly what ran. A script is not a
  transaction, and a partial application says which statement failed and how
  many had already run.
- **Admission is the engine's decision against the connection's capability
  profile**, not a tokenizer's: the `v1compat` profile refuses data-modifying
  subqueries and CTEs outright, while the session profile admits them and
  leaves them to the `WHERE` guard on their own merits. Transaction-control
  and session-state statements (`BEGIN`, `SET`, `LOCK`, `PRAGMA`) are refused
  off a session — on a pooled connection they would leave their state behind
  for whoever gets that connection next.
- Unterminated strings, comments and quotes are rejected as malformed rather
  than guessed at.
- Scripts over the size cap are rejected *before* execution.

These are the **syntactic** shapes — the ones a machine can be certain about.
They catch the classic accidents (`DELETE FROM orders` with the `WHERE` still
in your head) but they cannot tell a legitimate migration from a Friday-evening
mistake that happens to be well-formed.

**AI inspection of the SQL — your model, your keys, never your data.**
*(Designed, not yet implemented.)* On top of the
deterministic gates, an AI agent reviews the **statement text** and flags or
rejects dangerous executions that are syntactically perfect but semantically
alarming — the well-formed `DELETE` against the wrong table, the migration
nobody meant to run in production.

Two properties define the design:

- **It reads scripts, not rows.** This is an architectural line autodb
  enforces and *does not let you configure away*: the inspector has no access
  to database data. It never receives introspection objects; it gets a
  dedicated schema DTO restricted to an allowlist of identifier metadata —
  table and column names, type names, nullability, primary-key membership —
  which **structurally cannot represent** column defaults, function bodies or
  arguments, comments, or any expression text, because those carry literal
  values. There is no code path to result rows, to connections, to tools, or
  even to raw error text (server errors can embed values). Canary
  serialization tests plant secret-like content in every excluded field and
  assert it can never appear in a prompt.
- **Bring your own model.** autodb ships the seam, not the model, and provides
  no inference of its own. Point it at a **local SLM or LLM** via Ollama for a
  fully offline, nothing-leaves-the-box deployment, or supply **your own API
  keys** for Anthropic, OpenAI or another provider, or run a frontier model
  inside your own cloud boundary via Bedrock or Vertex. Providers are pinned
  **per connection**, so a sensitive database can stay local-only while others
  use a hosted model. API keys are stored by reference and sealed with the same
  argon2id/AES-GCM keyslot as every other secret. The shipping default is
  **off** — you turn it on deliberately.

Enforcement and provider are separate axes: a deployment runs `advisory`
(observe and annotate the audit) before it runs `enforcing` (refuse), and
rolling back means returning to advisory, never going blind. Every external
call is audited — provider, model digest, prompt revision, payload hash — so
"a production query went to a vendor" is never an unrecorded event.

**The honest caveat**, because a security tool should state it: statement text
can contain literal values in a `WHERE` clause, and enabling a hosted provider
means that text leaves your network. autodb makes that choice deliberate,
per-connection, defaulted off and fully audited — it does not make it for you.
A local model avoids it entirely.

And the boundary itself never moves: **a model verdict is probabilistic and is
never the security boundary.** That remains grants, server-enforced read-only
transactions, and the deterministic gates above. The AI is a net over them, not
a replacement for them.

**Audit trails.** Every executed statement is recorded with the user, the
connection, the SQL, the timing, the row count and the outcome. Refusals are
audited too — a blocked query is evidence, not a silence. The front door adds
token-attributed session opens and audited timeout rollbacks.

**Secrets at rest.** Connection credentials are sealed with AES-256-GCM under a
key unwrapped from your passphrase via argon2id (RFC 9106 profile). The
key-encryption key is never stored. Lose the meta store and the DSNs are
unrecoverable — which is the point: treat it as a credential store.

**Network posture.** Loopback by default everywhere. IP allowlisting at both a
global and a per-user layer. The browser UI refuses a non-loopback bind without
TLS. The front door validates its TLS material *before* it binds, and will not
listen with an identity it cannot prove.

## Databases, and adding more

Targets today: **PostgreSQL**, **MySQL**, **SQLite**. DSNs are validated on the
way in, and settings that would change parsing semantics under the classifier's
feet — `sql_mode`, `standard_conforming_strings`, `init_command`, disabled
autocommit — are rejected rather than silently honoured.

Target access goes through [`golib/dao`](https://github.com/yongjohnlee80/golib),
which owns the dialect and driver abstraction. That layer already ships a
**BigQuery** driver alongside the PostgreSQL and MySQL ones, and its
read-mostly / no-transaction driver contract exists precisely so warehouse-shaped
targets — no interactive transactions, different introspection, different
quoting — fit without special-casing them in autodb. **Wiring BigQuery in as an
autodb target is planned**; the abstraction it needs is already there.
