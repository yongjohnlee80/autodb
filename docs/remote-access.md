# Remote access: the TUI on your own computer

autodb's TUI can run on a developer's own computer and reach a remote autodb
server. Nobody needs a shell account on the server for this. The connection is
SSH to autodb's **own** SSH listener, which carries autodb only: no shell, no
`psql`, no port forwarding.

It is off until an admin turns it on. Who can connect is decided by the SSH
public keys registered on their autodb profile: there are no invites.

## For a developer

1. **Send your SSH public key to an admin**: the one line of your `.pub` file,
   for example `~/.ssh/id_ed25519.pub`. Ed25519 and ECDSA keys are accepted,
   and RSA keys of at least 3072 bits. If you can already sign in to that
   server's autodb some other way, you can add it yourself under
   **Home › My SSH keys…**. That asks for your autodb passphrase.
2. **Add the server** on your computer: **Remote › Manage… › Servers › Add**,
   with a name, the host, the port (blank for 7422), your autodb user there, and
   your SSH key file. This is kept in `remotes.toml` (`~/.config/autodb/` on
   Linux, `~/Library/Application Support/autodb/` on macOS), which holds no
   secret.
   - A key file **with a passphrase** must be served by `ssh-agent`: choose
     *ssh-agent* as the signing, and keep the key file's path (its `.pub` names
     the key). The TUI does not ask for an SSH key's passphrase itself.
3. **Connect**: **Remote › Connect…**, choose the server, and enter your
   **autodb** passphrase (not your SSH key's).
   - The **first** connect from a computer asks for it twice. A wrong entry
     counts as a refused attempt, and three refusals in a row block your
     network for 24 hours.
   - It then shows the server's **host key fingerprint**. Check it against the
     one your admin sent you before choosing **Trust**. From then on a
     different key is refused.
   - The first connect **enrolls this computer** as your key's device. The
     same key from another computer is refused: each computer uses its own key.

From then on, **Remote › Connect…** asks for your passphrase once, and a dropped
connection resumes by itself for a couple of minutes. **Remote › Disconnect**
signs out and goes back to this computer's daemon. Your notes, workspaces and
history while connected are the **server's**; nothing from this computer is
carried over, and nothing from the server stays behind.

To start straight on a server: `autodb --ui --remote <id>`, where `<id>` is the
profile's id in `remotes.toml` (for example `prod-db` for a server named
*Prod DB*). Or set it as the default in your config:

```toml
[tui]
start = "remote:prod-db"   # or "ask", or "local" (the default)
```

**When something is refused:**

| You see | What happened | What to do |
|---|---|---|
| *the server's host key is not the one pinned for this profile* | the server's host key changed, or this is not your server | ask your admin. Only if they confirm the change: **Servers › Forget host key**, and have an admin revoke your old device (its key was sealed to the old host key). |
| *this machine's device key is not the one this SSH key enrolled, or it was revoked* | the key's device is another computer, or the device was revoked | another computer: use a key of its own on this one (each key has one device). Revoked: **Servers › Remove**, then **Add** it again; the next connect enrolls this computer afresh. |
| *the ssh key … has a passphrase* | the key file is encrypted | choose *ssh-agent* for the server's signing, with the key loaded in your agent |
| the connection is refused or times out | Remote Control is off, the firewall does not let your address in, or your network is blocked after three refusals | ask your admin; a block can be lifted under **Blocked IPs** |

**New devices are announced.** At your next sign-in on any other computer, autodb
shows "New device enrolled from <IP> on <date> — not you? Revoke it."
**Revoke it** revokes that device and the SSH key it used. If it was not you,
change your autodb passphrase too: it was used.

## For an admin

1. **Open the port.** In the cloud firewall, allow inbound **TCP 7422** to the
   server, from your developers' addresses if you can. `provision_vm.sh` prints
   this rule at the end of a run. It never opens a port itself.
2. **Turn on Remote Control**: in the TUI, **Remote › Manage… › Remote Control
   › Turn on**. It shows the server's **host key fingerprint**: send it to each
   developer with the address, over a channel they trust.
3. **Register keys**: **System › Users** → a user → **SS(H) keys** → **Add key**.
   Paste their `.pub` line; no passphrase is asked when you add to someone else's
   profile.

What the Manage dialog shows an admin:

- **Remote Control** — on or off, where it listens (or why it is retrying),
  the host key, live remote connections, and **"Remote connections paused —
  denials cannot be recorded"** with its reason, if refusals cannot be written.
  New remote connections are refused while it says so. Turning it off ends
  every remote connection at once, an admin's own remote session too.
- **Devices** — every user's SSH keys and the device each is bound to, with
  **Revoke key** and **Revoke device**. Revoking ends that key's or device's
  connections at once. A revoked device leaves its key free for a new
  computer; a revoked key cannot connect at all.
- **Blocked IPs** — addresses with refusals counted against them (an IPv4
  address, or an IPv6 /64), the time a block has left, and **Unblock**.
- **Remote activity** — every device enrolled, device seen from a new IP
  address, and refused connection, with its address. Each user sees their own
  enrollments and new addresses.

Restarting the server is for operators on the host only: a remote session is
never offered it, whatever its role. A live remote session counts as busy, so an
idle restart waits for it.

### Moving developers off shell accounts

Once every developer connects through Remote Control, **remove their OS
accounts** on the server and keep shell access to operators. Before that, check
that each one has signed in remotely at least once (**Remote activity** shows
their enrollment), and that nobody's work depends on a file in their home
directory there.

## Configuration

Remote Control's switch lives in the store and is turned on and off from the
TUI. The `[remote]` section of [`config.example.toml`](../config.example.toml)
holds only the mechanics:
- `bind` (default `0.0.0.0:7422`) and `host_key`;
- `max_unauthenticated` and `handshake_timeout`;
- `block_after_failures` (3) and `block_duration` (24h);
- `reconnect_grace` (2m), how long a dropped connection may resume without the
  passphrase;
- `device_key_max_age` (90d), after which a computer's device key is replaced on
  its next connect.

For how the remote surface is defended, see [Security model](security.md), and
the decision records ADR-0207 and ADR-0208.
