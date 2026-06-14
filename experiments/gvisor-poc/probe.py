#!/usr/bin/env python3
"""Phase 7 Stage 4 — gVisor adversarial probe helper (TEMPORARY, not committed as
a permanent test). Posts a single program to a live goboxd /run endpoint and prints
the build/run result. Used to spot-check the gVisor sentry boundary (/proc + /sys
synthesis completeness, env-var inheritance, process visibility, strace-log growth,
cross-mechanism memory/network) for docs/gvisor-security-assessment.md.

Usage:
    python probe.py <lang> <source-file> [url]    # url default http://127.0.0.1:8090
    echo '<source>' | python probe.py <lang> - [url]
"""
import json, sys, urllib.request

def main():
    lang = sys.argv[1]
    src_arg = sys.argv[2]
    url = sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:8090"
    src = sys.stdin.read() if src_arg == "-" else open(src_arg, encoding="utf-8").read()

    req = {"language": lang, "source": src,
           "tests": [{"stdin": "", "expected_stdout": ""}]}
    # c/cpp/java/verilog need source+artifact filenames; supply sane defaults.
    defaults = {
        "c":   ("solution.c", "solution"),
        "cpp": ("solution.cpp", "solution"),
        "java": ("Main.java", "Main"),
        "verilog": ("solution.v", "solution.vvp"),
    }
    if lang in defaults:
        req["source_filename"], req["artifact_filename"] = defaults[lang]

    body = json.dumps(req).encode()
    r = urllib.request.urlopen(urllib.request.Request(
        url + "/run", data=body, headers={"Content-Type": "application/json"}),
        timeout=120)
    out = json.load(r)
    if out.get("build"):
        b = out["build"]
        print(f"[build] status={b['status']} stderr={b['stderr'][:400]!r}")
    for i, t in enumerate(out.get("tests", [])):
        print(f"[run]   status={t['status']} dur={t['duration_ms']}ms mem={t['memory_peak_kb']}kb")
        print(t["stdout"])
        if t["stderr"]:
            print("--- stderr ---")
            print(t["stderr"][:1500])

if __name__ == "__main__":
    main()
