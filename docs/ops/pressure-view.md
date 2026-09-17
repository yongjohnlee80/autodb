# Seeing what the front door is holding

When clients are being refused and nobody can say why, this is the surface that
answers it: sessions and leases against their caps, what has been refused in the
last minute split by whether it was *us* or *them*, and which source addresses
are being held out.

**The reason this page exists is not that the tunnel is hard.** During the
incident this work came from, the pool was full for a sustained period and the
figure that explained it was available the whole time. Nobody looked, because
nothing said this was the way in.

## Reaching it

The web surface binds to loopback **only**, on purpose, and that is not going to
change: a status surface on a public interface is a different security posture
than this daemon has chosen. So reaching it from your machine is an SSH tunnel.

One command, from your own machine:

```
ssh -N -L 7010:127.0.0.1:7010 <operator>@<autodb-host>
```

Then open <http://127.0.0.1:7010/> and press `SPC P`, or use **System →
Pressure** from the menu bar.

- `-N` because you want the forward and not a shell.
- `7010` is the default `--web-ui` port. If it was started with a different
  `--port`, change **the right-hand** number to match and leave the
  left-hand one alone unless it collides with something local.
- The right-hand `127.0.0.1` is resolved **on the remote host**, which is why
  this reaches a loopback-only bind at all.

## What you need to be

**The view is admin-only.** It reports other people's session counts and the
source addresses being refused, so an ordinary editor account cannot open it.
If the pane says it is unavailable, check the account before checking the
daemon.

## Reading it

- **`n / m` is occupancy against a cap.** A row rendered `no limit` means that
  dimension has no cap configured — it is not a breach.
- **Rows marked as raised have crossed their threshold**, and the same crossing
  is in the journal and the audit trail. Raising and clearing use *different*
  numbers deliberately, so a figure sitting near the line does not flap.
- **Every refusal shows its class.** `capacity` means we ran out of room;
  `credential` means somebody failed to authenticate. They are answered in
  opposite directions — resizing a pool does nothing about password guessing,
  and vice versa.
- **`and N more`** means the list is capped for display. The number is the true
  remainder, not the number of rows hidden on this screen.
- **A throttled source shows how much longer it has.** That is when the throttle
  actually lifts, not when its first failure expires.

## If the pane says it is unavailable

It is telling you it could not read, which is deliberately different from
showing you a calm, empty table. In order of likelihood: the account is not an
admin; the front door is not enabled on that install; or the daemon is not
reachable from the session you are in.
