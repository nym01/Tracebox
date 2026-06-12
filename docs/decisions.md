# Design decisions

Short log of non-obvious sandbox decisions and the reasoning behind them, per the
project's "be able to explain it in an interview" rule.

## D1 — cpp filesystem isolation: two profiles, deliberately asymmetric (Batch B)

cpp makes **two** nsjail invocations per request — the g++ build step and the
`./solution` run step — and they get **different** mount sets:

- **Build step (`/usr/bin/g++`) — intentionally broad.** g++ is a driver that
  shells out to `cc1plus`, `as` and `ld`, and reads a large, version-specific set
  of files (C/C++ headers, crt objects, link-time libraries). Rather than
  enumerate every file, the build profile mounts whole directories read-only —
  the header search paths, the gcc install dir and the link-time library
  directories — all discovered by asking g++ itself (`-E -v`,
  `-print-search-dirs`, `-print-prog-name`), plus the driver/assembler/linker
  binaries and the dynamic loader. In practice this reduces to read-only mounts of
  `/usr/lib`, `/lib`, `/usr/include` (and the toolchain binaries + loader).

  This is acceptable because at build time the untrusted input is the *source*,
  and the compiler is trusted. Exposing system headers and libraries **read-only**
  grants no capability the source couldn't already exercise through a normal
  compile, and there is no network in the jail (nsjail's default new net
  namespace). The build sandbox still cannot see `/etc`, `/home`, `/root`, the
  host `/proc`, `/var`, the host `/tmp`, etc.

- **Run step (`./solution`) — intentionally minimal.** This is the
  security-critical step: it executes the untrusted *binary*. It mounts only the
  individual shared objects the binary needs (libstdc++, libm, libgcc_s, libc and
  the loader) — no directories — plus the per-request work dir. Verified: a
  compiled program that tries to open `/etc/passwd` gets "file not found", because
  `/etc` does not exist in the run sandbox.

### Why the run-step libraries can be cached at startup

The binary is per-request content, but its *library dependencies* are static for
our flag allow-list (`-O*`, `-Wall`, `-Wextra`, `-std=*` — none change linkage,
and a request cannot inject `-l` flags). So instead of `ldd`-ing every request's
binary, startup compiles a tiny probe `.cpp`, `ldd`s it once, and caches the
result. glibc 2.34+ (bookworm ships 2.36) folded libpthread/libdl into libc, so
even programs using `std::thread` need no extra library beyond the cached set.

### Why directories mount at their literal path, not the symlink-resolved one

On Debian `/lib` is a symlink to `/usr/lib`. The `libm.so` **linker script**
hardcodes `/lib/x86_64-linux-gnu/libm.so.6`, so the sandbox must expose that exact
path. The mount set therefore binds the real source onto each *literal* requested
path (`/usr/lib/...:/usr/lib/...` and `/usr/lib/...:/lib/...`) rather than
collapsing both to the single real path — otherwise the literal `/lib/...`
reference dangles and `ld` fails with "cannot find libm.so.6".

### Why the build step gets a writable tmpfs `/tmp`

g++ writes intermediate `.s`/`.o`/`.res` files under `/tmp`. With a fresh mount
namespace there is no `/tmp` unless we add one, so the build profile mounts a
writable tmpfs at `/tmp` (`--tmpfsmount /tmp`). The interpreted and run profiles
deliberately get **no** `/tmp` — they don't need scratch space, and the run step
should have nothing it doesn't need.

### c comes along for the run step, not the build step

c's run step also invokes `./solution`, so it already rides the
compiled-artifact run profile (a strict security improvement — c binaries are now
isolated at runtime). Only c's `gcc` build step remained on the shared filesystem
until Batch C (below) migrated it. This asymmetry is intentional and harmless: the
C++ run-library set is a superset of what a C binary needs.

## D2 — c filesystem isolation: gcc build reuses the g++ machinery (Batch C)

The gcc build step (`/usr/bin/gcc`) now gets the same isolated mount profile as
g++: a fresh mount namespace with the compiler toolchain (driver, cc1, as, ld),
its header and library search dirs and the dynamic loader bind-mounted read-only,
a writable tmpfs `/tmp` for intermediate files, and the per-request work dir
writable. The resolution logic is shared: `resolveCompilerBuildMounts(compiler,
lang)` is parameterized by driver path and source language, and the cpp/c build
resolvers are thin wrappers over it. The three probe helpers (`compilerIncludeDirs`,
`compilerSearchDirs`, `compilerProgPath`) were parameterized by compiler path so
both drivers reuse them.

c needs **no** separate run profile: `./solution` dispatches to the existing cpp
run profile, and a C binary links a subset of what the C++ probe pulls in, so the
cached C++ run set is a safe superset.

## D3 — java filesystem isolation: mount the whole JDK, symlink the launcher (Batch D)

java makes **two** nsjail invocations per request — the `javac` build step and the
`java` run step — but, unlike cpp/c, they are **not** asymmetric broad-vs-minimal.
Both get essentially the same profile, because both are JVM launchers and both need
the entire JDK **runtime image** (`lib/modules`, `lib/server/libjvm.so`, the JDK's
own `lib/*.so`, and `conf/`) to start at all. There is no "minimal run set" to
carve out the way there is for a self-contained compiled C/C++ binary — the thing
being executed (`java`) *is* the full runtime every time.

Because the run launcher is the same `/usr/bin/java` every request (the per-request
content is the `.class` file in the writable work dir, not the executable), **both**
java profiles are static and resolved once at startup — no per-request `ldd`, even
for the run step. This is actually simpler than cpp, whose run step had to probe a
representative binary.

Each profile mounts the whole `JAVA_HOME` tree (e.g.
`/usr/lib/jvm/java-17-openjdk-amd64`) read-only at its literal path. Like the
compiler build profile, this is broad but acceptable: the JDK is trusted, only the
source/`.class` is untrusted, the tree is read-only, and there is no network in the
jail. The build step additionally gets a writable tmpfs `/tmp` (the JVM writes
`/tmp/hsperfdata_*`); the run step deliberately gets none — the JVM degrades
gracefully without it, and the security-critical step should have nothing spare.

Verified end-to-end in Docker: java hello world builds and runs (`accepted`); a
program that `Files.readAllBytes(/etc/passwd)` gets `NoSuchFileException`
(`runtime_error`) because `/etc/passwd` does not exist in the sandbox; a syntax
error returns `build_failed`; and py3/bash/js/cpp/c/verilog all still return
`accepted`.

### Why a `--symlink`, not a bind mount, for `/usr/bin/java`

The interpreter profiles bind the real binary onto the invocation path
(`realpython:/usr/bin/python3`). That would **break** java. The JDK launcher derives
`JAVA_HOME` at startup by reading `/proc/self/exe` and stripping the trailing
`bin/<launcher>`. A bind mount onto `/usr/bin/java` makes `/proc/self/exe` report
`/usr/bin/java`, so the JVM mis-derives `JAVA_HOME` as `/usr`, fails to find
`/usr/lib/modules`, and aborts ("Error occurred during initialization of VM").

So instead we recreate the original symlink inside the sandbox with nsjail's
`--symlink JAVA_HOME/bin/java:/usr/bin/java`. `execve("/usr/bin/java")` then resolves
through the symlink to the real path under the mounted `JAVA_HOME`, and the
derivation lands on the correct home — exactly as it does unsandboxed on Debian,
where `/usr/bin/java` is itself an update-alternatives symlink chain.

### Two non-obvious extra mounts the launcher's own `ldd` does not reveal

Getting java to start took two iterations beyond "mount JAVA_HOME + symlink", both
because the launcher binary's `ldd` under-reports what the JVM actually loads:

1. **`libjvm.so`'s dependencies.** `libjvm.so` is `dlopen`'d by the launcher at
   runtime, so `ldd /usr/bin/java` does not list its transitive dependencies. The
   first failure was `dl failure ... libstdc++.so.6: cannot open shared object
   file` — `libstdc++`, `libgcc_s` and `libm` live in the system lib dir, not under
   `JAVA_HOME`. Fix: also `ldd` `JAVA_HOME/lib/*/libjvm.so` and fold its system
   libraries into the mount set (`jvmLibraries` globs for it so a non-`server` VM
   layout still resolves).

2. **Debian's `/etc/java-*-openjdk` config tree.** Debian splits the editable JDK
   config (`java.security`, `logging.properties`, …) out into
   `/etc/java-17-openjdk` and symlinks `JAVA_HOME/conf/**` at it. Those symlinks are
   *inside* the mounted `JAVA_HOME`, but their targets are not, so the JVM aborted
   with "Error loading java.security file". Fix: also mount the `/etc/java-*-openjdk`
   directory read-only (`javaEtcConfigDirs` globs for it, so a version bump still
   resolves and a non-Debian JDK that keeps config inside `JAVA_HOME` needs no extra
   mount). Note this mount is what creates `/etc` in the java sandbox — but only
   `/etc/java-17-openjdk` exists under it, never `/etc/passwd`, so the runtime
   `/etc/passwd` read still fails as required.

### verilog is the last language still on the shared filesystem

After Batch D only verilog (`iverilog` build, `vvp` run) still uses
`--disable_clone_newns`. It is the remaining migration before the filesystem-
isolation gap is fully closed. (Closed in Batch E — see D4.)

### gcc vs g++ search paths — what actually differs

Asking both drivers on the bookworm image (gcc/g++ 12):

- **Include dirs:** gcc omits the three C++ header directories g++ adds
  (`/usr/include/c++/12`, `/usr/include/x86_64-linux-gnu/c++/12`,
  `/usr/include/c++/12/backward`). The other four (`/usr/lib/gcc/.../include`,
  `/usr/local/include`, the multiarch `/usr/include/...`, `/usr/include`) are
  identical.
- **`-print-search-dirs`** (install dir + link-time libraries): **byte-for-byte
  identical** between gcc and g++.

So gcc's build mount set is a strict subset of g++'s, and after the mountSet
ancestor-reduction (where `/usr/include` already subsumes the C++ subdirs) the two
resolved sets collapse to effectively the same mounts. Verified end-to-end in
Docker: c hello world builds and runs (`accepted`); a C program that `fopen`s
`/etc/passwd` gets a NULL handle (`runtime_error`, prints "nope") because `/etc`
does not exist in either the build or run sandbox; a malformed C source returns
`build_failed`; and py3/bash/js/cpp/java/verilog all still return `accepted`.

## D4 — verilog filesystem isolation: mount the ivl base dir, shell only for build (Batch E)

verilog makes **two** nsjail invocations per request — the `iverilog` build step
(which produces `solution.vvp`) and the `vvp` run step (which executes it). With
Batch E both move into their own mount namespace, and verilog leaves
`--disable_clone_newns`. **No language is left on the shared filesystem now**; the
`--disable_clone_newns` default branch only catches a genuinely unknown command.

Like java, both verilog steps share one resolver (`resolveVerilogMounts`) and both
are static and cached at startup — the run launcher is the same `vvp` every request
(the per-request content is the `.vvp` artifact in the writable work dir), so there
is no per-request `ldd`. This was simpler than java: no `dlopen`'d runtime image to
chase the way `libjvm.so` was, and **no launcher symlink trick** — Icarus's tools
find their support files from a compiled-in base path (overridable with `-B`), not by
deriving a home from `/proc/self/exe`, so a plain bind mount onto `/usr/bin/iverilog`
/ `/usr/bin/vvp` works.

### Both steps mount the whole Icarus "ivl base directory"

On bookworm the base dir is the multiarch `/usr/lib/x86_64-linux-gnu/ivl` (found by
globbing `/usr/lib/ivl*` and `/usr/lib/*/ivl*` and validating by content — it holds
the `ivl` binary and `*.conf` files — so a distro/layout change doesn't silently
break the mount). It contains the programs `iverilog` shells out to (`ivl`, `ivlpp`),
the `*.tgt` code generators `ivl` dlopens (`vvp.tgt` emits the `.vvp`), and the
`*.vpi` modules `ivl`/`vvp` load (`system.vpi` provides `$display`, `$finish`, …).
Both steps mount this whole tree read-only at its literal path — broad but, like the
compiler-build and JDK profiles, acceptable: the Icarus install is trusted, only the
source/`.vvp` is untrusted, the tree is read-only, and there is no network in the jail.

### Two non-obvious requirements the launcher's own `ldd` does not reveal

Getting verilog to run took two iterations beyond "mount the base dir + the launcher's
libraries", both because the launcher binary's `ldd` under-reports what actually runs:

1. **`/bin/sh` for the build step.** `iverilog` is a driver that invokes `ivlpp` and
   `ivl` via `system()`, i.e. through the shell. Without `/bin/sh` (dash) the build
   exits 127 with empty stderr (`build_failed` and a confusing silent failure). Fix:
   the build profile binds the real dash onto `/bin/sh` and `ldd`s it. The **run
   profile deliberately omits the shell** — defense in depth, so the sandbox executing
   untrusted compiled code has no shell to reach even via Icarus's `$system()` task.

2. **The VPI/target modules' own libraries.** `system.vpi` drags in
   `libbz2`/`libz`/`libstdc++` that neither launcher links directly (the modules are
   exec'd or `dlopen`'d, so `ldd` of `iverilog`/`vvp` misses them). The first run failed
   to load `system.vpi` ("`libbz2.so.1.0`: cannot open shared object file"). Fix:
   additionally `ldd` every module file in the base dir (`ivl`, `ivlpp`, `vhdlpp`,
   `*.tgt`, `*.vpi`) and fold in its libraries, best-effort (a non-dynamic file `ldd`
   refuses is skipped, never failing startup). Because `vvp` loads `system.vpi` at
   runtime too, the run step gets `libbz2`/`libz` as well — necessary, not spare.

Verified end-to-end in Docker (Icarus 11.0): verilog hello world builds and runs
(`accepted`); a program that `$fopen`s `/etc/passwd` at runtime gets a 0 handle and
prints "nope" (`accepted`) because `/etc` does not exist in the run sandbox; a syntax
error returns `build_failed`; `$system("true")` does not reach a shell; and
py3/bash/js/cpp/c/java all still return `accepted`, with all three endpoints 200.

## D5 — seccomp syscall filter: a uniform kafel deny-list (Phase 1, step 2)

The sandbox now applies a seccomp-BPF filter (`configs/seccomp.policy`, compiled by
nsjail's kafel) to **every** language's child process via `--seccomp_policy`. It is
the same policy for all seven languages and both the build and run steps — wired
into the shared base args (`buildNsjailArgs`), not `filesystemArgs` — so there are
**no per-language seccomp branches**. The policy path is resolved (and the file's
existence checked) once at startup in `NewNsjailRunner`, so a missing/mis-deployed
policy fails loudly at boot rather than per request, exactly like a failed mount
resolution.

### Why a deny-list, not an allow-list

Phase 1 deliberately ships a **conservative deny-list** (`DEFAULT ALLOW`, `KILL` a
fixed set of dangerous syscalls) rather than a default-deny allow-list. Seven very
different runtimes (CPython, V8/node, the JVM, g++/gcc toolchains, Icarus, dash)
each issue a wide and version-dependent syscall set; an allow-list tight enough to
be worthwhile is also tight enough to silently break one of them, and getting it
exhaustively right is a separate, later hardening step. The deny-list instead closes
the specific escape / host-tampering primitives untrusted code has no legitimate
reason to use, while leaving normal compile/run syscalls (open, read, write, mmap,
clone, execve, …) untouched. `KILL` (SECCOMP_RET_KILL) means the task dies the
instant it makes a denied call, so the handler observes it as the program dying —
`runtime_error` — not as an ignorable error return.

The denied set (per notes.md): `ptrace`, `bpf`, `mount`, `umount`(2), `kexec_load`,
`init_module`, `finit_module`, `delete_module`, `reboot`, `swapon`, `swapoff`,
`setns`, and dangerous `unshare`.

### Why denying these in the child does not break nsjail's own setup

nsjail performs its namespace/mount/cgroup setup in the **parent**, before the
seccomp filter is installed on the child (just prior to `execve`). So denying
`mount`, `setns` and `unshare` to the child does not interfere with nsjail creating
the mount namespace or applying the read-only bind mounts — it only stops the
untrusted program from undoing them. Verified: all seven hello-worlds still build
and run (`accepted`) with the filter active.

### `unshare` is filtered on its argument, not blanket-killed

A blanket `unshare` KILL would be broader than "dangerous usage" — `unshare(2)` has
harmless flags (e.g. `CLONE_FILES`, `CLONE_FS`). The dangerous ones are new-namespace
creation, above all `CLONE_NEWUSER` (a new user namespace hands the caller a full
capability set inside it — the classic privilege-escalation primitive) and
`CLONE_NEWNS` (a new mount namespace). kafel can match on syscall arguments, so the
rule is `unshare { (unshare_flags & (CLONE_NEWUSER | CLONE_NEWNS)) != 0 }`. Verified
in Docker: a C program calling `unshare(CLONE_NEWUSER)` is killed (`runtime_error`,
no output), while `unshare(CLONE_FILES)` runs to completion (`accepted`, prints
"survived").

### Two kafel-syntax gotchas worth remembering

1. **Comments are `//` and `/* */`, not `#`.** A leading `#` is reserved for
   `#define`; a `#`-style comment makes kafel fail with "unexpected IDENTIFIER,
   expecting DEFINE" and nsjail then aborts ("Couldn't prepare sandboxing policy").
2. **The syscall is named `umount`, not `umount2`.** kafel's amd64 table maps the
   name `umount` to syscall 166 — which *is* `umount2` on x86-64 (there is no legacy
   `umount`). Writing `umount2` gives "Undefined identifier `umount2`".
   `CLONE_NEWUSER`/`CLONE_NEWNS` likewise have no built-in names and are `#define`d
   to their stable kernel ABI values in the policy.

### Verifying the ptrace kill: C, not py3+ctypes

notes.md's suggested check is a py3 program that calls `ptrace` via `ctypes`. Under
the current py3 filesystem profile that test returns `runtime_error`, but for the
**wrong reason**: `ctypes` fails to import because `libffi.so.8` is not in the py3
mount set, so `ptrace` is never reached — the failure proves nothing about seccomp.
The kill was therefore verified with a compiled C program that genuinely issues the
syscall: `ptrace(PTRACE_TRACEME,0,0,0); printf("survived")` returns `runtime_error`
with empty stdout (killed before the print), while the identical program **without**
the `ptrace` line returns `accepted` and prints "survived" — isolating the kill to
the `ptrace` call. (Making the py3+ctypes route demonstrate this too would require
adding `libffi` to the py3 mount profile — a filesystem-isolation change, out of
scope for the seccomp step.)

## D6 — cgroup v2 memory enforcement: resident, not virtual (Phase 1, step 3)

`memory_exceeded` was in the status vocabulary but never enforced. The hackathon's
`--rlimit_as` was dropped because it caps **virtual** address space, which the JVM
and V8 reserve far in excess of their real footprint — any `rlimit_as` tight enough
to matter aborts them at startup ("Could not reserve enough space for object heap",
"Failed to reserve virtual memory for CodeRange"). The fix is a cgroup v2
`memory.max` limit, which tracks **resident** memory: the managed runtime can reserve
its huge virtual space and still be OOM-killed the instant its real footprint exceeds
the language's `memory_kb` budget.

### Wiring (uniform, like seccomp)

When `spec.MemoryKB > 0`, `buildNsjailArgs` appends `--use_cgroupv2 --cgroup_mem_max
<MemoryKB*1024> --cgroup_mem_swap_max 0` to the **shared base args** (not
`filesystemArgs`), so it applies identically to all seven languages and to both the
build and run steps, with no per-language branch. nsjail then creates a fresh cgroup
`<cgroupv2_mount>/NSJAIL.<pid>` for the child and writes `memory.max`. The container
must run with **`--cgroupns=host`** so nsjail can reach the host cgroup v2 hierarchy
and enable the memory controller in the root `cgroup.subtree_control`; without it
every request fails. Documented in the README and `docker-compose.yml`
(`privileged: true`, `cgroup: host`).

### Why swap must be pinned to 0

`memory.max` alone is a **soft** cap: when RSS hits the limit the kernel first spills
pages to swap, and only OOM-kills when swap is also exhausted. Under Docker Desktop
(WSL2 has a swapfile) the Node.js bomb just thrashed swap until the wall clock and was
misreported as `time_exceeded`. Setting `memory.swap.max = 0`
(`--cgroup_mem_swap_max 0`) makes the limit a hard RSS cap, so exceeding `memory.max`
triggers the OOM killer immediately. (py3's single ~8 GiB allocation overwhelmed even
swap fast enough to OOM without this, which is why it worked before the flag and
masked the gap.)

### Detecting the OOM kill: the exit code, because the cgroup is already gone

The obvious detector — reading the cgroup's `memory.events` `oom_kill` counter — is
**unusable here**. nsjail's `reapProc` calls `cgroup2::finishFromParent` (an `rmdir`
of `NSJAIL.<pid>`) the instant it reaps the child, *before* the nsjail parent exits.
By the time our `cmd.Wait()` returns the cgroup directory no longer exists, so there
is nothing to read. Detection therefore uses the **exit code**: nsjail propagates a
signalled child as `128 + signo` (subproc.cc), and a cgroup OOM kill is SIGKILL → exit
**137**. `oomKilled(exitCode, timedOut, memLimitKB)` returns true only when
`exitCode == 137 && !timedOut && memLimitKB > 0`. The handler maps that to
`memory_exceeded`, checked **before** the generic non-zero-exit → `runtime_error`
branch (an OOM kill is also a non-zero exit, but more specific).

### Why 137 is unambiguous here despite timeout sharing it

Exactly two things SIGKILL the child: the cgroup OOM killer and nsjail's own
`--time_limit` at the wall deadline. Both yield 137, but timeouts are already detected
separately (run duration ≥ wall limit, or the outer-context deadline), so a 137 that
is *not* a timeout is the OOM killer. Nothing else collides: a seccomp `KILL` is
SIGSYS → 159, and an ordinary program failure is its own non-zero code. Verified that
a py3 infinite loop still returns `time_exceeded` (not `memory_exceeded`) with the
limit active.

### What the three memory bombs actually exercise (and a JVM/Node subtlety)

Verified end-to-end in Docker (`--privileged --cgroupns=host`): py3 (`x=[0]*10**9`),
js, and java memory bombs all return `memory_exceeded`, while all seven hello-worlds
return `accepted` (the limits don't break normal programs that fit their budget), the
py3 `/etc/passwd` read still fails, a cpp build error is `build_failed`, and a py3
infinite loop is `time_exceeded`. Two non-obvious things surfaced while picking the
bomb payloads — both about the *workload*, not the sandbox:

1. **A memory bomb must write non-zero bytes to be resident.** `Buffer.alloc(2GB)` in
   Node was reported `accepted`: `alloc` zero-fills, but the kernel backs read-only
   zero pages with a single shared zero page, so RSS never grows. The reliable Node
   bomb is `Buffer.allocUnsafe(n).fill(1)` (or growing an on-heap array), which faults
   in private dirty pages. py3's `[0]*10**9` works because it's ~8 GiB of list
   pointers, all resident.
2. **An on-heap Java bomb may hit the JVM's own limit first.** With container support
   the JVM sizes its heap from the cgroup limit, so a pure heap bomb can throw
   `OutOfMemoryError` (→ `runtime_error`) before RSS reaches `memory.max`. Empirically,
   under the 512 MiB run budget and `swap.max=0`, the array bomb does cross
   `memory.max` (transient GC copies) and is cgroup-OOM-killed → `memory_exceeded`; a
   native `Unsafe.allocateMemory`+`setMemory` bomb (off-heap, not bounded by `-Xmx`)
   is the unambiguous way to drive resident memory past the cap and was confirmed to
   give `memory_exceeded` too. This is the case `--rlimit_as` fundamentally could not
   handle: it killed the JVM at startup; the cgroup lets it start and kills it only
   when real memory is actually over budget.
