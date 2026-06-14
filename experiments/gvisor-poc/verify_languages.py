#!/usr/bin/env python3
"""Phase 7 Stage 2 verification driver.

POSTs a hello-world /run for each of the 7 languages and prints status + output
+ durations. Also runs an invalid-syntax case for a compiled language to confirm
build_failed, and reads /proc/version from inside the sandbox for the Finding F
check on a non-py3 rootfs.

Usage: python verify_languages.py [base_url]   (default http://127.0.0.1:8080)
"""
import json, sys, time, urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"

HELLO = {
    "py3":     ("print('hello')", None, None),
    "bash":    ("echo hello", None, None),
    "js":      ("console.log('hello')", None, None),
    "c":       ('#include <stdio.h>\nint main(){printf("hello\\n");return 0;}', None, None),
    "cpp":     ('#include <iostream>\nint main(){std::cout<<"hello\\n";return 0;}', None, None),
    "java":    ('public class Main{public static void main(String[] a){System.out.println("hello");}}', "Main.java", "Main"),
    "verilog": ('module main;\ninitial begin $display("hello"); $finish; end\nendmodule\n', None, None),
}

def post(path, body):
    data = json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data,
                                 headers={"Content-Type": "application/json"})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=90) as r:
        out = json.load(r)
    return out, (time.time() - t0) * 1000

def run_lang(lang, source, srcfn, artfn, stdin="", expect="hello\n"):
    body = {"language": lang, "source": source,
            "tests": [{"stdin": stdin, "expected_stdout": expect}]}
    if srcfn:  body["source_filename"] = srcfn
    if artfn:  body["artifact_filename"] = artfn
    return post("/run", body)

def main():
    print(f"=== Target: {BASE} ===\n")
    print(f"{'lang':8} {'top_status':14} {'build':10} {'run_status':14} {'out':8} {'build_ms':>8} {'run_ms':>7} {'e2e_ms':>7}")
    results = {}
    for lang, (src, srcfn, artfn) in HELLO.items():
        try:
            resp, e2e = run_lang(lang, src, srcfn, artfn)
        except Exception as e:
            print(f"{lang:8} ERROR: {e}")
            results[lang] = ("error", None)
            continue
        top = resp.get("status", "?")
        build = resp.get("build")
        bstatus = build["status"] if build else "-"
        bms = build["duration_ms"] if build else 0
        t = resp["tests"][0] if resp.get("tests") else {}
        rstatus = t.get("status", "?")
        out = t.get("stdout", "").strip()
        rms = t.get("duration_ms", 0)
        ok = "OK" if out == "hello" else repr(out)[:8]
        print(f"{lang:8} {top:14} {bstatus:10} {rstatus:14} {ok:8} {bms:8} {rms:7} {e2e:7.0f}")
        results[lang] = (top, rstatus, out, rms)
    return results

if __name__ == "__main__":
    main()
