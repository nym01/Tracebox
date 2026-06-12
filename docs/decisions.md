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
