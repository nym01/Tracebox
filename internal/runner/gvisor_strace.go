package runner

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nym01/goboxd/internal/tracer"
)

// This file parses gVisor's `runsc --strace` output into the SAME tracer.Event
// shape the eBPF tracer produces, so a gVisor run's audit trail flows into
// trace_events identically to an nsjail run's — even though the collection
// mechanism is entirely different (host eBPF tracepoints for nsjail vs. the
// sentry's own syscall log for gVisor).
//
// # Why strace, and why parse text
//
// Under gVisor the guest's syscalls are serviced by the sentry and never reach
// the host kernel tracepoints the eBPF tracer attaches to (POC §5), so that
// tracer captures nothing for a gVisor run. gVisor's own equivalent is
// `runsc --strace`, which logs every guest syscall from inside the sentry. It is
// emitted as text into runsc's debug log (the component log whose name contains
// "boot"); there is no structured (e.g. JSON/protobuf) strace channel in runsc —
// the structured alternative is the heavier Runtime-Monitoring trace-points API,
// deferred as future work. So this stage parses the text log, which is stable
// enough for the three syscalls the eBPF tracer also captures.
//
// # Line format (from gVisor pkg/sentry/strace)
//
// Each traced syscall produces an ENTER ("E") line on entry and an EXIT ("X")
// line on return, embedded in a debug-log line whose prefix we ignore:
//
//	D0610 12:34:56.789012   42 strace.go:567] [   1: 1] python3 E openat(AT_FDCWD /work, 0x7f8 /work/solution.py, O_RDONLY|O_CLOEXEC, 0o0)
//	D0610 12:34:56.789300   42 strace.go:625] [   1: 1] python3 X openat(...) = 3 (34.278µs)
//
// We parse only the ENTER ("E") line. That is the deliberate analogue of the
// eBPF tracer, which attaches to the sys_enter_* tracepoints: it records the
// *intent* of the syscall, before the result. It also means a connect() under
// network=none — which fails ENETUNREACH inside the sandbox — is still recorded
// (the destination the code wanted to reach), exactly as the eBPF tracer records
// it for nsjail (see internal/tracer/doc.go's connect note).
//
// Argument rendering, quoted from the gVisor formatters:
//   - a path argument is "%#x %s"  -> "0x7f8 /work/solution.py" (hex addr, space,
//     then the unquoted path);
//   - the execve argv vector is "%#x [%q, %q, …]" -> `0x.. ["python3", "x.py"]`
//     (hex addr, then a bracketed list of Go-quoted strings);
//   - a sockaddr is "%#x {Family: %s, Addr: %v, Port: %d}" ->
//     "0x.. {Family: AF_INET, Addr: 8.8.8.8, Port: 53}".
//
// The regexes below target each shape directly rather than splitting the whole
// arg list, because paths and argv blobs can themselves contain commas/spaces.
// Any line that does not match is skipped, never fatal — a format drift degrades
// to "fewer events captured", it does not break a run (the SIMPLIFIED-v1 contract
// the task allows for fragile text formats).
var (
	// straceEnterRe locates the ENTER line for the four syscalls the eBPF tracer
	// also captures and isolates the argument list. The leading " E " is the
	// strace enter marker (exit lines use " X "); anchoring the trailing ")" to
	// end-of-line keeps us on enter lines (exit lines end with "= ret (dur)").
	straceEnterRe = regexp.MustCompile(` E (openat2?|execve(?:at)?|connect)\((.*)\)$`)

	// openatPathRe pulls the path (2nd arg) out of an openat/openat2 arg list:
	// "<dirfd>, 0x<addr> <path>, <flags>, <mode>". [^,]+ stops at the next arg
	// boundary; a path containing ", " is the only case this truncates (rare, and
	// it yields a shortened path, not a failure).
	openatPathRe = regexp.MustCompile(`^[^,]*, 0x[0-9a-fA-F]+ ([^,]+)`)

	// execvePathRe pulls the filename (1st arg) out of execve: "0x<addr> <path>, …".
	execvePathRe = regexp.MustCompile(`^0x[0-9a-fA-F]+ ([^,]+),`)

	// execveatPathRe pulls the pathname (2nd arg) out of execveat:
	// "<dirfd>, 0x<addr> <path>, …".
	execveatPathRe = regexp.MustCompile(`^[^,]*, 0x[0-9a-fA-F]+ ([^,]+),`)

	// argvListRe captures the contents of the FIRST bracketed list in the arg
	// string, which for execve/execveat is argv (envp is a later bracket). [^\]]*
	// keeps the match within a single list.
	argvListRe = regexp.MustCompile(`\[([^\]]*)\]`)

	// argvElemRe matches each Go-quoted ("%q") element of an argv list, honouring
	// backslash escapes so embedded quotes/spaces survive.
	argvElemRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

	// connectAddrRe captures the destination of an AF_INET/AF_INET6 sockaddr. Other
	// families (AF_UNIX, …) render without Addr/Port and simply do not match, so —
	// like the eBPF tracer — only IP connects produce an event.
	connectAddrRe = regexp.MustCompile(`\{Family: AF_INET6?, Addr: ([^,]+), Port: (\d+)\}`)
)

// collectStraceEvents reads the strace events runsc wrote for one run. runsc emits
// the guest's strace into the debug-log component whose filename contains "boot"
// (gvisorDebugLogTmpl uses the %COMMAND% substitution), so we glob bundleDir for it
// rather than hard-coding the substituted name, which is robust to runsc's exact
// naming. Every match is parsed and the events concatenated. It is best-effort: a
// missing log (strace was off, or runsc failed before logging) yields no events,
// never an error — the audit trail degrades to empty, exactly as it does for an
// nsjail run the eBPF tracer happened to miss.
func collectStraceEvents(bundleDir string) []tracer.Event {
	matches, err := filepath.Glob(filepath.Join(bundleDir, "*boot*"))
	if err != nil {
		return nil
	}
	var events []tracer.Event
	for _, path := range matches {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		events = append(events, parseStraceEvents(f)...)
		f.Close()
	}
	return events
}

// parseStraceEvents reads a runsc strace/debug log and returns the file_open,
// exec and connect events it records, in log (chronological) order. The returned
// events carry the same Kind/Syscall/Path/Argv/DestIP/DestPort fields the eBPF
// tracer sets, so downstream emit/persist code is source-agnostic. Time is set to
// the parse instant (when user space read the log); the relative order is the
// log's order, which is what the store preserves.
func parseStraceEvents(r io.Reader) []tracer.Event {
	var events []tracer.Event
	sc := bufio.NewScanner(r)
	// strace lines can be long when argv/env blobs are dumped; raise the line cap
	// well above bufio's 64 KiB default so a long line is parsed, not dropped.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r\n")
		m := straceEnterRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		syscall, args := m[1], m[2]

		switch syscall {
		case "openat", "openat2":
			if pm := openatPathRe.FindStringSubmatch(args); pm != nil {
				events = append(events, tracer.Event{
					Kind:    "file_open",
					Syscall: syscall,
					Path:    strings.TrimSpace(pm[1]),
					Time:    time.Now(),
				})
			}

		case "execve", "execveat":
			ev := tracer.Event{Kind: "exec", Syscall: syscall, Time: time.Now()}
			pathRe := execvePathRe
			if syscall == "execveat" {
				pathRe = execveatPathRe
			}
			if pm := pathRe.FindStringSubmatch(args); pm != nil {
				ev.Path = strings.TrimSpace(pm[1])
			}
			if lm := argvListRe.FindStringSubmatch(args); lm != nil {
				for _, tok := range argvElemRe.FindAllString(lm[1], -1) {
					if s, err := strconv.Unquote(tok); err == nil {
						ev.Argv = append(ev.Argv, s)
					} else {
						ev.Argv = append(ev.Argv, strings.Trim(tok, `"`))
					}
				}
			}
			events = append(events, ev)

		case "connect":
			if cm := connectAddrRe.FindStringSubmatch(args); cm != nil {
				port, _ := strconv.Atoi(cm[2])
				events = append(events, tracer.Event{
					Kind:     "connect",
					Syscall:  "connect",
					DestIP:   strings.TrimSpace(cm[1]),
					DestPort: port,
					Time:     time.Now(),
				})
			}
		}
	}
	return events
}
