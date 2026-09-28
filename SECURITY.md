# Security policy

autodb stands between people and production databases, so a security bug in
it matters. Thank you for taking the time to report one.

## Reporting a vulnerability

**Please do not open a public issue, discussion or pull request.** Report it
privately through GitHub instead:

**[Report a vulnerability](https://github.com/yongjohnlee80/autodb/security/advisories/new)**
(the repository's **Security** tab → **Report a vulnerability**)

The report stays private between you and the maintainer until a fix is
released. Please include:

- the autodb version (`autodb --version`) and the platform
- which surface is involved: the front door, the RPC server, the terminal or
  browser UI, the Neovim plugin, or one of the install scripts
- steps to reproduce, or a proof of concept
- what an attacker gains, and what they need first (a network position, an
  account, a role, a token)

autodb is maintained by one person. I aim to acknowledge a report within a
week, and to agree a disclosure date with you once the problem is understood.
You will be credited in the advisory unless you ask not to be.

## Supported versions

Security fixes go into the newest release line only. Please upgrade before
reporting, if you can.

| Version | Supported |
|---|---|
| 0.4.x | yes |
| older | no |

## Scope

In scope:

- getting past a gate: a role, a connection grant, the read-only
  transactions, the statement guards, the IP allowlist or a token's limits
- recovering a connection secret or the master key from the meta store, a
  backup or a process
- authenticating to the front door or the RPC server without valid
  credentials, or using a revoked or expired token
- an action that reaches a target database without an audit record
- the install, update and uninstall scripts doing something other than what
  they print, or deleting outside their stated targets

Known limits, documented rather than vulnerabilities:

- **The RPC server has no login rate limiting yet.** On a loopback port, any
  account on the host can attempt logins. A unix socket (mode `0600`) is the
  stronger boundary. It is the default for a personal install and for
  `install_frontdoor.sh`; `provision_vm.sh` uses a port so that a team can
  mint its own tokens, and `--rpc-socket` opts out. See
  [Who can reach the daemon](docs/configuration.md#who-can-reach-the-daemon).
- **Running the front door without TLS** (`insecure_disable_tls`) sends every
  token in cleartext. The setting is named to say so.
- **Statement text can contain values.** A future AI review that you point at
  a hosted model would send statement text to that provider. It is not
  implemented yet, and it is designed to be off by default and chosen per
  connection.

How the gates work is described in [docs/security.md](docs/security.md).
