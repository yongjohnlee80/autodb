# Front door: operator reference

Everything the scripts do, and why. For a guided walkthrough start with the
[tutorial](tutorial.md); this page is the reference behind it.

## How the front door behaves

- **Credentials are named Personal Access Tokens, never passwords.** One per
  machine or app (`auth.token_create "work-laptop"`), listed and revoked
  individually and instantly. Tokens are stored as a selector plus a SHA-256
  hash, capped (16 per user, 512 global), expire after 90 days by default and
  365 days at most, and
  can carry their own IP allowlist that must be a subset of the user's. Your
  login passphrase never goes in a DSN — it unwraps your encryption keyslot.
- **Read and write, gated by role.** `editor` users run the application's real
  traffic through the full gate stack. `reader` users are pinned inside
  server-enforced read-only transactions.
- **Transactions behave like PostgreSQL.** One open transaction per connection;
  a connection pool holds several, bounded by per-user session caps. Abandoned
  transactions cannot sit on production locks forever: a session idle inside a
  transaction is rolled back after `idle_in_tx_timeout` (default **2 h**), and no
  transaction outlives `max_tx_duration` (default **8 h**). Both live in `[exec]`
  in [`config.example.toml`](../../config.example.toml). Every timeout rollback
  is audited with the limit that fired.
- **Refusals explain themselves.** An autodb-layer refusal carries an accurate
  SQLSTATE, the gate rule in `DETAIL`, and the fix in `HINT` — rendered
  natively by psql and IDEs. Target errors pass through verbatim, so you always
  know *which layer* said no.

## Updating an install

[`update_frontdoor.sh`](../../update_frontdoor.sh) resolves the **newest release tag**
first, builds that, swaps the binary and restarts the unit. It touches the
binary and the unit only — the config, the meta store, the TLS material and the
unattended-unlock keyslot are neither read nor written, because an update is not
a reinstall.

```sh
sudo sh update_frontdoor.sh --check        # installed vs available; changes nothing
sudo sh update_frontdoor.sh                # do it
sudo sh update_frontdoor.sh --ref v0.3.6   # a specific tag, e.g. to go back
```

The binary lives at `/usr/local/bin/autodb` (`--prefix` moves it); the config is
`/etc/autodb/`. Those are separate on purpose, and it is why an update can
replace one without touching the other.

**It rolls back.** `systemctl restart` returns success for a unit that starts
and then exits immediately, so "the restart worked" is not evidence the daemon
is running. The previous binary is kept at `/usr/local/bin/autodb.previous`, the
new one has to reach `ActiveState=active`, and if it does not the old binary
goes back and the service is restarted on it. An update that leaves the front
door down is worse than no update.

## Provisioning this machine

Give `provision_vm.sh` a loopback host and it provisions **the machine you are
on**, over no transport at all — no sshd, no key, no login:

```sh
sudo sh provision_vm.sh --apply --host 127.0.0.1
```

`localhost` and `::1` do the same. `--user` is not required there, because
there is nothing to log in to. Everything else is identical: the same sizing
preflight, the same interview, the same first-run ceremony, and the closing
notes print commands you can run directly rather than wrapped in `ssh`.

## Running the front door as a service

`install.sh` installs the binary. [`install_frontdoor.sh`](../../install_frontdoor.sh)
configures the PostgreSQL-wire front door as a **systemd service** on a host
that already has it: a memory-sizing preflight, a config scaffold, an optional
PostgreSQL meta store, and a unit with `Restart=on-failure`, `GOMEMLIMIT` and
`MemoryMax`.

```sh
sh install_frontdoor.sh --check                     # measure and report; changes nothing
sh install_frontdoor.sh --check --assume-ram 1024 --assume-cpus 1
sh install_frontdoor.sh --print-config              # the config it would write, to stdout
sudo sh install_frontdoor.sh --apply                # prompts for each setting on a terminal
```

`--apply` walks the whole bring-up rather than stopping at a config file. It
asks for a **DNS name** — leave it empty and the certificate is issued for an
**IP address** instead — then the **port**, because together those are what
somebody types into a client. With an address to issue for it runs
`autodb --create-cert`, writes the certificate paths in, and enables the
surface. Then it runs [`autodb --init`](#running-the-front-door-as-a-service)
for the first administrator and the unattended-unlock slot, and starts the
service if you asked it to.

That leaves one file to hand out: `ca.pem`, plus `sslmode=verify-full` in the
client's DSN. **`SPC k` in the TUI shows that certificate's contents** — not
its path, which is no use to a developer on another machine, and unreadable
even on the host unless you are root or the service account. It opens in a
read-only vim editor: select and `y` to copy part of it, `Y` for the whole
thing, and the footer names the keys.

For an internet-facing deployment prefer a real ACME certificate and pass
`--no-cert`.

`--check` is the default and never writes anything. `--apply` interviews you,
pre-filling every answer with the computed default, so pressing return through
the whole thing gives exactly the non-interactive result.

### Why there is a sizing preflight

The front door's memory budgets are **accounting, not allocations**, and nothing
in autodb reads a physical-memory figure — there is no `GOMEMLIMIT`, no
`MemAvailable` check. A daemon whose general lane is larger than its host will
boot happily, idle at a fraction of it, and then be unable to apply backpressure
before the kernel's out-of-memory killer arrives. The guard is present, is
consulted, and observes nothing.

So the preflight computes a lane and a session cap the host can actually honour,
and refuses a host too small to serve even one session. `--assume-ram` and
`--assume-cpus` size a machine you are not standing on, which is how a small VPS
gets planned from a workstation. **Pass both** — with only the first, the CPU
warnings describe your workstation and the single-core warning silently never
fires for the VM you were sizing for.

The two numbers move **together**: the general lane's floor is
`max_sessions_global × 4 MiB`, so raising the cap without raising the lane fails
at startup. Lowering the *cap* is how a modest host asks for a smaller lane — it
serves fewer sessions rather than promising more than it can hold.

### The meta store, as one choice of three

autodb's own database — users, grants, encrypted connection secrets, the audit
log. Not a database you connect *to*.

| Choice | What it does |
|---|---|
| `sqlite` | One file, no server. The lighter choice on a small VPS. |
| `pg-local` | Installs PostgreSQL here, creates the database and a role named after the service account so peer auth over the unix socket needs no stored password. |
| `pg-remote` | An existing instance, by DSN. |

A co-hosted PostgreSQL is charged its **own** reserve, and sizing is recomputed
after that answer rather than before it — on a 1 GB host it takes the front door
from 64 sessions to 32, and at 512 MB it is refused outright.

### What to be aware of

- **`--apply` is proven on ONE package manager, and the other four are
  untested.** It has run end to end on a Debian-family host — a DigitalOcean
  droplet, 1 vCPU / 961 MiB, through provisioning, TLS issuance, the first-run
  ceremony and a working JDBC client — so the **apt-get** branch is exercised,
  not merely written.

  The other four (dnf/yum, pacman, apk, zypper) remain a best effort at each
  distro's conventions and **not a support claim**: a run on one distro says
  nothing about the other four, and the RHEL family, Arch and Alpine also ship
  PostgreSQL's cluster uninitialised, which that run never touched. Use a
  disposable VM for `--apply` on anything but a Debian-family host. The script
  header carries a per-distro status table.

- **The sizing figures are provisional policy, not measurement.** Only the 4 MiB
  watermark, the 256 default cap and the 1 GiB/4 GiB lane bounds come from the
  code. The reserve fraction, the lane share and the PostgreSQL allowance are
  conservative guesses chosen to fail toward a smaller front door. The header
  documents how to replace them with real RSS figures.
- **TLS is mandatory, so the front door ships disabled.** `enabled = true`
  without both TLS keys is refused at config load — the daemon would not start at
  all — so a config written before you have certificates sets `enabled = false`.
  Add the `tls_*` keys and flip that line in the same edit. Without TLS a client
  using `sslmode=require` authenticates nothing, and an active MITM collects
  every access token in cleartext; a token works from anywhere it is admitted
  until revoked.
- **Migration from sqlite to PostgreSQL is ONE-WAY.** Decide before there is
  production data.
- **A PostgreSQL meta store's transport is checked at startup.** The DSN needs
  `sslmode=verify-full` with an explicit `sslrootcert`, or autodb refuses to
  start. `require` encrypts but authenticates nothing, and an absent `sslmode`
  means libpq's `prefer`, which silently falls back to plaintext. The one
  exception is a genuinely local channel — a unix socket or same-host loopback —
  via the deliberately named `allow_insecure_dsn`.
- **Developer self-service PAT minting needs the RPC endpoint on a port.**
  `provision_vm.sh` uses a loopback port (7419) unless you pass
  `--rpc-socket`; `install_frontdoor.sh` run on its own defaults to the socket
  and takes `--rpc-port` to opt in.

  On a socket, the endpoint is a file at mode `0600`, re-applied on every bind — the socket file *is* the access control,
  and a socket peer is exempt from the IP allowlist because reaching it already
  proves same-user access. On a host where autodb runs as a service that socket
  belongs to the service account, so **only it and root can reach the TUI** —
  and the TUI is where a developer mints their own token
  (`auth.token_create` authorises any authenticated user, bound to a connection
  they hold a grant on). On a socket, every credential request is a root
  operation.

  `--rpc-port` (7419) trades that for self-service. Be clear about the trade:
  it replaces "same OS user" with "allowlist plus login", and **there is no
  rate limiting on the RPC surface** — no connection, pre-auth or auth-failure
  throttle exists there, and autodb's own config calls TCP M9-gated pending TLS
  and rate limits. Every local account can then reach it and attempt logins. It
  binds `127.0.0.1`, so nothing is reachable off-host either way. Choose it
  when developer self-service is worth that on a box whose own accounts you
  trust; `provision_vm.sh` makes that choice for you unless you pass
  `--rpc-socket`.

  In port mode the installer also writes a world-readable `client.toml`
  carrying the address, `client_only = true`, and nothing else — so a developer
  can run `autodb --ui --config /etc/autodb/client.toml` without reading the
  server config, which may name a PostgreSQL DSN with a password in it.
  `client_only` matters on its own: without it, running the TUI while the
  service is down would start a daemon as *them*, against their own empty meta
  store, on the port the real service binds. It is no longer the only guard —
  **no frontend spawns a daemon on a host that has a service config at all**,
  whichever config it holds, including the service's own. That file used to be
  excluded, and a TUI run against it spawned a detached daemon on the service's
  own ports that outlived the TUI and kept the unit from starting.
- **Run `autodb --init` once** to create the first administrator and cut the
  unattended-unlock slot. It is the only surface that can do the second part:
  enrolling the slot is admin-only *and* only possible while the store is
  unlocked, because wrapping the master key requires holding it — and
  bootstrapping generates that key, so the token and the unlocked store arrive
  together in one process. It takes the instance lease, so stop the service
  first. Re-running is safe: an existing slot is reported, never replaced.
- **Set up unattended unlock, or a reboot locks everyone out.** Connection
  secrets are encrypted with a master key normally unwrapped by a passphrase at
  login, so after a restart every front-door client gets
  `57P03 "the server is not accepting connections"` until a human logs in by
  hand. `autodb --init` (which `install_frontdoor.sh --apply` runs) cuts the
  slot for a new store. For a store that already exists, set `service_keyfile`
  and cut it **once** from a running, unlocked daemon: `autodb --ui`, then
  `SPC K`, then `e`. There is no `keyslot` subcommand, because enrolment is
  admin-only and only possible while the store is unlocked. Give the keyfile
  **its own directory** — a keyfile beside the meta store
  means one careless `tar` captures both halves of the envelope — and it must be
  `0600`.
- **`ip_allowlist` is loopback-only by default**, enforced at login, so nothing
  remote can log in until you widen it. Widen it deliberately and narrowly.
- **A connection is not reachable until `frontdoor_exposed = true`.** Exposure
  is a separate, deliberate step and does not change its SQL capability profile.
- **On a 1 vCPU host, interactive logins are slow.** Front-door PAT auth is
  SHA-256 and cheap, but passphrase login uses argon2id at `m=64 MiB, p=4` — four
  parallel lanes serialized onto one core, each transiently allocating 64 MiB.
  TLS handshakes land on that same core.
- **Small VPSes usually ship with no swap.** Add some regardless; the daemon's
  budgets assume headroom the kernel does not otherwise have.

### Removing it

[`uninstall.sh`](../../uninstall.sh) removes the service, its config, its state and
its service account, and `--remove-swap` / `--remove-toolchain` also undo what
`provision_vm.sh` added underneath.

```sh
sh uninstall.sh --check                             # list what would go; changes nothing
sh uninstall.sh --print-targets                     # the exact deletion set, one path per line
sh uninstall.sh --backup-only                       # take the archive and stop
sudo sh uninstall.sh --apply
sudo sh uninstall.sh --apply --remove-swap --remove-toolchain
```

`--print-targets` is worth running before any `--apply`: it prints every path
the script would unlink, so the destructive surface is something you read
rather than infer. `--backup-only` stops the service, takes the archive, and
removes nothing.

**It archives the meta store first, and the archive deliberately excludes the
service keyfile.** That store holds the encrypted connection secrets, and the
master key that opens them lives only in its own keyslot envelope — there is no
other copy, so deleting it destroys those secrets permanently. But putting the
store *and* the keyfile in one tarball would be both halves of the envelope in a
single file, which turns a backup into a credential. The store is archived at
`0600`, the keyfile is left where it is, and the archive carries a `README`
saying so. `--no-backup` skips the archive entirely.

**Nothing is deleted unless the archive is good.** Every copy must succeed and
the finished tarball is listed back and checked for the store by name; any
failure aborts with the sources untouched and says so. A backup whose whole
purpose is to make the following deletion survivable must not be allowed to be
silently partial.

**A config value cannot aim the deletion at your system.** Paths that come from
the config must sit at least two levels inside a short allowlist of places a
meta store legitimately lives — `/var/lib`, `/var/opt`, `/srv`, `/opt`,
`/usr/local/share`, your home directory — and the config itself must contain
recognisable autodb sections before the script treats it, or anything it names,
as ours. That is an allowlist rather than a denylist because a denylist cannot
enumerate every system file worth protecting.

A PostgreSQL meta store is **not** touched: dropping a database is not an
uninstaller's decision to make.

### Provisioning a fresh VM

[`provision_vm.sh`](../../provision_vm.sh) is the layer beneath the installer: it
takes a **fresh** Linux VM over SSH and brings it to a built, configured front
door. It owns the machine — swap, base packages, a `mise`-managed Go toolchain,
cloning and building autodb — then hands off to `install_frontdoor.sh` for the
service itself rather than duplicating it.

```sh
./provision_vm.sh --user root --host 203.0.113.10          # probe only; changes nothing
./provision_vm.sh --user root --host vm.example.com --apply
./provision_vm.sh root@203.0.113.10 --apply --meta pg-local
```

`--check` is the default: it connects, measures the host, prints the plan and
the front-door sizing that host would get, and exits. Every step is idempotent,
so a re-run repairs rather than duplicates.

**It adds swap on small hosts, for a measured reason.** Compiling autodb peaks
near 700 MiB of RSS in a single compile process even fully serialized
(`modernc.org/sqlite` is the heavy one). On a 1 GB VPS with no swap — the
default on most providers — the Go compiler gets OOM-killed. Disk is the cheap
resource there, so it trades some for a build that finishes, and the swap keeps
earning its place afterwards. `--prebuilt` cross-compiles locally and uploads
the binary instead, for hosts too small even with swap.

It installs the newest patch release in the Go **minor line** that `go.mod`
requires, not the exact figure written there. That line is a minimum language
version, not a toolchain pin, so installing it literally would build with the
oldest compiler the module permits — and a stdlib that old carries advisories in
`crypto/tls` and `crypto/x509` that a TLS-terminating front door should not be
shipping with.

**Validated on Ubuntu 24.04** (1 vCPU / 961 MiB) for the sqlite backend. The
PostgreSQL install paths are still untested. An `--apply` run issues the TLS
certificate and starts the service; the start is withheld only when there is
no TLS material, for example with `--no-cert` before you have installed your
own — see the caveats above.
