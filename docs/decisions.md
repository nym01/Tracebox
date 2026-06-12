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
