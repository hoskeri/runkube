# Experience: dynamic file injection via a transferred rootfs fd

Date: 2026-08-30
Task: `docs/TODO.md` → Sandbox "Dynamic file injection" (implementing unit:
`pkg/sandbox`)

## 1. The change

The TODO asked for three things:

1. The child hands the parent an fd on the container rootfs in the setup
   response.
2. Files injected into the rootfs from sandbox config data (the rendered
   `/sys/devices` tree + write-type payloads) should be created through that
   fd, so the model owns them.
3. Result: file contents can be modified at runtime, not just at container
   start.

What shipped:

- **New `Rootfs` type** (`pkg/sandbox/rootfs.go`): wraps the transferred root
  directory fd. `Write` builds the file as a uniquely-named sibling
  (`.rk-inject-<pid>-<n>`), writes it fully with `fchmod`-fixed mode, then
  `renameat`s it over the target; `MkdirAll` walks `mkdirat` per component.
  All paths are absolute container paths converted to rootfd-relative form
  (`cleanRel`), so injection never touches the parent's mount namespace and
  keeps working after the child pivots.
- **Child** opens the rootfs dir (`O_RDONLY|O_DIRECTORY|O_CLOEXEC`) right
  after mounting the tmpfs — before the seccomp filter, with the rest of the
  descriptor setup — and transfers it in the setup response as `rootfs-fd`.
  It also uses the same handle locally for its own boot-file materialization.
- **`Sandbox.bootFiles()`**: one renderer for config-derived regular files
  (sysfs tree, then write-type payloads with the child's newline/mode
  normalization inline). Both sides render from this single list, so the two
  copies are byte-identical by construction.
- **Parent** receives `rootfs-fd` in `Run()`, keeps it on the `Sandbox` for
  the sandbox's lifetime, and immediately injects `bootFiles()` through it.
  Runtime surface: `s.Rootfs().Write(...)` for one file,
  `s.InjectFiles(...)` for model-rendered batches.
- Write payloads are no longer separate from sysfs materialization; bind
  payloads stay child-side (they are real mounts, not files).
- Unit tests in `rootfs_test.go` cover the fd mechanics without any sandbox:
  parent-dir creation, truncate-replace, empty content, idempotent mkdir,
  path validation/escape clamping, bootFiles encoding, and the runtime API.

## 2. Why writes are temp+rename, not open+truncate

Two failure modes force atomic renames:

- **The EACCES race.** The child and parent materialize the *same* boot files
  concurrently (the parent starts as soon as it sees the setup response; the
  child proceeds in its boot path — there is deliberately no second
  roundtrip, per the Single Roundtrip contract). Sysfs files are 0444. If the
  parent `openat(O_WRONLY|O_TRUNC)`s a file the child just created, or vice
  versa, the owner gets EACCES and the sandbox dies at startup. `rename(2)`
  needs only write permission on the *directory*, so two concurrent writers
  can race safely, and the last identical rename wins.
- **Runtime updates of read-only files.** The point of the feature is
  rewriting the 0444 sysfs tree after boot. A plain `O_WRONLY` open of an
  existing 0444 file fails for the non-root parent; rename does not.

A bonus: readers inside the container see either the old or the new content,
never a torn file.

## 3. Lessons

- **The TODO's `os.MkdirAt(rootfsFd, "/sys/devices/node0")` sketch is wrong in
  one important way**: `*at` syscalls ignore `dirfd` for *absolute* pathnames.
  Passing absolute paths naively would inject into the caller's real root.
  The correct shape strips the leading `/` and resolves dirfd-relative
  (optionally with `path.Clean` first, which clamps `..` at the top and makes
  escapes impossible by construction).
- **When two processes are designed to write the same files, they must be
  byte-identical by construction, not by convention.** The shared
  `bootFiles()` renderer (including the payload newline/default-mode
  normalization that previously lived only in the child's `write()`) is what
  makes the double materialization safe. Any divergence would be an
  unreproducible flake, not a hard failure.
- **Concurrent identical writes are a correctness problem even when both
  writers are "the same"** — the parent's copy and the child's copy are
  separate processes with separate state, racing on the same inodes. The
  previous session's lesson (ordering invariants are load-bearing; do not
  re-add roundtrips) directly motivated keeping a child-side boot path and
  making the write mechanism race-proof instead.
- **`os.File.Fd()` returns `uintptr` in modern Go** — pass it as `int` to
  `golang.org/x/sys/unix` or you get a type error at `go vet`/build time.
- **`t.TempDir()` files can be written through a read-only dirfd handle**:
  the test that writes over an existing 0444 file (asserting renaming works
  where a direct `O_WRONLY` open fails for non-root) is the regression test
  that pins the whole design.
- **A bug in path joining shows up as a confusing EACCES, not a path error**:
  `tmpInjectName` produced `/.<pid>-...` (absolute) via `dir = ""` + `"/"`,
  so `openat` ignored the dirfd and tried to create the temp at `/` — root
  of the real filesystem. The "permission denied" pointed the way, but the
  real fault was the leading slash. When a temp sibling's directory is the
  root, emit no `/` prefix at all.

## 4. Verification

- `make test` (full suite) and `go vet` clean; only pre-existing gofmt
  noise in `pkg/sandbox/seccomp/{handlers.go,internal/types.go}`.
- Standalone binary (`tools/isolated`) in an unprivileged userns: container
  sees parent-injected `/sys/devices/system/node/node0/meminfo`,
  `/sys/devices/system/cpu/online`, boot payload files — filter and
  no-filter paths both.
- Runtime injection, live: a scratch binary runs `sandbox.Run()` in a
  goroutine and, 800ms after rootfs exchange, rewrites
  `/sys/devices/system/cpu/online` (via `InjectFiles`) and `/etc/hello` (via
  `Rootfs().Write`). The busybox command inside the container then prints
  `cpu-online=0-3` and `hello=updated-at-runtime` — i.e. the running
  container observes the post-boot updates.
- Any failure in fd transfer or injection is loud: `Run()` returns an error
  and the sandbox fails to start, so the smoke test exercises the new path.

## 5. Notes / follow-ups

- Runtime injection currently has no caller besides the demo; the CRI model's
  hotplug-style events (CPU/memory changes) are the natural next consumer.
- Injection follows symlinks for intermediate path components (parity with
  open(2)); `openat2(RESOLVE_NO_SYMLINKS|RESOLVE_BENEATH)` would harden this
  if untrusted roots ever appear.
- `docs/TODO.md` "File IO improvements" (TRO/RW/RDWR emulation, backing files
  in rootfs or `/.emulated`) remains open; the `Rootfs` handle is a
  ready-made home for that work.