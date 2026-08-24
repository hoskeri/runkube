# Experience: collapsing the fdchan sandbox handshake into a single setup RPC

Date: 2026-08-30
Task: `docs/TODO.md` → Sandbox "Single Roundtrip" (implementing unit:
`pkg/sandbox`, `pkg/sandbox/fdchan`, `pkg/sandbox/seccomp`)

## 1. The change

The sandbox bootstrap previously used three separate fdchan request/response
roundtrips between the parent and the sandboxed child:

1. `proxy-listener` — child creates the TCP listener for its egress CONNECT
   proxy and passes the listener FD up.
2. `seccomp-filter` — child installs the seccomp user-notify filter and
   passes the notify-listener FD up (only when `Features.SyscallFilter`).
3. `complete` — child signals it is done with the coordination phase and the
   parent is free to proceed.

The TODO asked for one "setup" RPC: request carries metadata (and FDs)
parent→child, response carries metadata (and FDs) child→parent. The fdchan
transport already supported exactly this shape (`fdchan_test.go` did it), so
the work was orchestration, not transport.

What shipped:

- One `setup` request: parent sends the sandbox `Config` as JSON metadata
  (previously base64'd into argv as a "cookie"; the argv cookie is now a
  marker only).
- One response: `"setup-ok"` plus two descriptors — `proxy-listener` and
  (conditionally) `seccomp-fd`.
- The child proceeds with booting and execs the sandboxed command; the
  parent sorts the response FDs and launches the egress forwarder and the
  seccomp supervisor.
- Side task from the adjacent inline TODO: the hardcoded egress upstream
  (`[::1]:6443`) moved into `Config.Egress`.

All unit tests and `go vet` passed before the first live run. The first live
run deadlocked.

## 2. The bug: a cross-process lock-order inversion

The symptom: the sandbox hung. Parent produced no output at all; the child
was spinning. `strace -f` (with SIGURG noise filtered out) told the story:

- Parent: `futex(FUTEX_WAIT_PRIVATE, NULL)` — blocked on the fdchan RPC
  response channel.
- Child: an endless
  `uname → ERESTARTSYS → rt_sigreturn → uname → …` loop — the tracee had
  hit a seccomp-trapped syscall whose responder never came. Because the
  kernel replies by re-running the syscall, the thread "spins" instead of
  blocking: USER_NOTIF doesn't pause the tracee, it bounces it.

Root cause: in my refactor the child created the proxy listener **after**
installing the seccomp filter. Go's `net.Listen(":8443")` probes
`uname(2)` (MPTCP support detection) during socket setup. After the filter
is installed, that `uname` is trapped with `SECCOMP_RET_USER_NOTIF`, and the
child cannot answer until the parent's supervisor loop is running. The
parent only starts the supervisor *after* receiving the setup response.
Circular dependency:

```
parent blocks on setup response
  └── child blocks on trapped uname notification
        └── parent hasn't started supervisor (waiting on response)
```

The original three-roundtrip code never hit this because its ordering was
*implicitly safe*: create listener (no filter yet) → install filter → hand
off supervision. That invariant lived only in the sequence of roundtrips; no
comment stated it. Collapsing the handshake deleted the invariant silently.

The fix was not new logic — it was restoring the original ordering inside
the single roundtrip (listener first, then filter) and promoting the
invariant to an explicit, commented contract.

## 3. Lessons

### Go runtime behavior

- **The stdlib makes syscalls you don't plan for.** `net.Listen` calls
  `uname(2)` for MPTCP probing. Nothing in the sandbox code calls `uname`.
  When you intercept syscalls, you filter the Go runtime plus every library,
  not just your program. Any code that can run after filter install must be
  assumed to trap syscalls you never wrote.
- **Raw FDs and the GC are a footgun.** `*os.File.Fd()` returns an `int`
  that the runtime does not protect; the `*os.File` can be collected and the
  descriptor closed under you. The existing supervisor code defends itself
  with `runtime.KeepAlive` and "prevent GC" comments — borrowed from the
  repo's own prior pain. When transferring FDs across processes, ownership
  and lifetime on *both* sides matter.
- **SIGURG is everywhere.** The trace was dominated by
  `tgkill(1, 1, SIGURG)` — Go async preemption. The seccomp supervisor
  already handles its fallout (ENOENT/ESRCH from `SECCOMP_IOCTL_NOTIF_RECV`,
  retry on EINTR). Seeing it live made those defensive branches
  understandable instead of cargo-cult.

### Linux / seccomp

- **A trapped syscall with no responder looks like a busy loop, not a
  hang.** ERESTARTSYS → rt_sigreturn → retry. This is what debugging a
  USER_NOTIF deadlock looks like in a trace: CPU spinning, tiny stack.
- **Handshake ordering is load-bearing state.** The safety of the original
  code was an ordering property of roundtrips, invisible in any single call
  site. Simplifications that reorder or merge RPCs must first extract those
  implicit ordering invariants and then re-assert them explicitly in code.
- **`/proc/self/exe` re-exec bakes in environment assumptions.** The
  sandbox respawns its own binary. Under `go test` the sandbox failed with
  `fork/exec /proc/self/exe: permission denied` — an environment artifact of
  the test harness (and a `nosuid,nodev` /tmp). The real delivery artifacts
  (standalone binaries) ran fine. Lesson in knowing when NOT to chase a
  mechanism: the shipped binary is the authoritative test target.

### Testing

- **Unit tests cannot catch cross-process deadlocks.** Everything went
  green while the feature was completely broken. The regression signal was
  live execution: first run's silent instant exit vs. hang, plus a clean
  `git stash` baseline run of the original code for comparison.
- **Strace with `-f`, then ask "who is blocked where".** Per-pid tails of
  the last syscalls pointed at the exact culprit threads in minutes.
  Filtering the SIGURG noise was what made the signal visible.
- **A test can double as a specification.** `fdchan_test.go` was already a
  working single-roundtrip demo; reading it told me the target API before
  writing code.
- **Sandbox smoke tests need the manual-tag escape hatch.** Real namespace/
  mount/seccomp tests can't run in CI; marking them `//go:build manual` and
  recording the run recipe (standalone binary, non-`/tmp` build dir) keeps
  them reproducible by hand.

## 4. Practical rules going forward (in this repo)

1. Any sandbox setup code that runs after `InstallFilter()` must not call
   `net`/`os` runtime paths (or anything that may probe `uname`, MPTCP,
   resolvers); create sockets and files *before* filter install.
2. If the fdchan bootstrap is ever restructured again, the ordering
   invariant — trappable work before supervision starts — is a contract,
   not a detail.
3. Verify sandbox changes with the real `make all` binaries in an
   unprivileged userns BEFORE trusting the unit test suite.
4. When transferring an FD, keep the `*os.File` alive explicitly on both
   ends and close your local copy after the transfer completes.
5. `strace -f` + per-pid last-syscall inspection is the first debugging move
   for parent/child hangs, not added logging.