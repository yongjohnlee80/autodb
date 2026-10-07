# Cancelling a statement on a SQLite target

On a SQLite target, a cancel can occasionally be **lost**, and the statement
then runs to completion. This covers a cancel from the TUI, and a statement
deadline enforced through the context. A statement that never completes, such
as an unbounded recursive query, keeps running. PostgreSQL targets are not
affected: their cancel is a separate request to the server.

## Why

autodb reaches SQLite through the `modernc.org/sqlite` driver. When a
statement's context is cancelled, the driver calls `sqlite3_interrupt` once.
SQLite clears a pending interrupt when a statement starts and nothing else is
running. So an interrupt that lands after the driver has checked the context,
but before the statement's first step, is discarded. Nothing sends another.

The window is narrow and depends on timing. It was found in CI, where a test
cancelled an endless query and the test runner timed out.

## What to do

- If a SQLite statement will not stop, restart the daemon: the statement
  ends with the process.
- Prefer bounded queries on SQLite targets, for example with a `LIMIT` on a
  recursive query.

## The fix

The fix belongs in the driver: keep sending the interrupt until the call
returns. A patch and a deterministic test have been prepared for
`modernc.org/sqlite`. With it, a statement whose cancel lands before its first
step is interrupted within about ten milliseconds. This page goes away when a
driver release carrying the fix is adopted.
