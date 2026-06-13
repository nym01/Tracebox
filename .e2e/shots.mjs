// Capture screenshots of the Tracebox frontend for a visual review. Runs fully
// offline: it seeds localStorage with two realistic past runs (a py3 success and
// a cpp build_failed) so the real ResultPanel/History components render without
// needing the API, then captures the initial view and both result panels.

import { chromium } from "playwright";

const FRONTEND = process.env.FRONTEND_URL ?? "http://localhost:5173";
const OUT = "shots";

const py3 = {
  id: "seed-py3",
  timestamp: Date.now() - 60000,
  language: "py3",
  snippet: 'print("hello from frontend")',
  statusLabel: "Ran",
  status: "accepted",
  runId: "0190f3a2-7b1c-7e44-9c2a-1f2e3d4a5b6c",
  source: 'print("hello from frontend")\n',
  stdin: "",
  response: {
    run_id: "0190f3a2-7b1c-7e44-9c2a-1f2e3d4a5b6c",
    status: "accepted",
    tests: [
      {
        status: "accepted",
        stdout: "hello from frontend\n",
        stderr: "",
        duration_ms: 14,
        memory_peak_kb: 9216,
      },
    ],
  },
};

const cpp = {
  id: "seed-cpp",
  timestamp: Date.now() - 30000,
  language: "cpp",
  snippet: "int main() {",
  statusLabel: "Build failed",
  status: "build_failed",
  runId: "0190f3a2-9d2e-7f55-a1b3-2c4d5e6f7a8b",
  source: "int main() {\n  this is not valid c++\n  return 0\n}\n",
  stdin: "",
  response: {
    run_id: "0190f3a2-9d2e-7f55-a1b3-2c4d5e6f7a8b",
    status: "build_failed",
    build: {
      status: "failed",
      stdout: "",
      stderr:
        "solution.cpp: In function 'int main()':\n" +
        "solution.cpp:2:8: error: expected ';' before 'is'\n" +
        "    2 |   this is not valid c++\n" +
        "      |        ^~\n" +
        "solution.cpp:2:3: error: 'this' is unavailable for static member functions\n",
      duration_ms: 233,
    },
    tests: [
      { status: "not_executed", stdout: "", stderr: "", duration_ms: 0, memory_peak_kb: 0 },
    ],
  },
};

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });

// Seed history before the app loads.
await ctx.addInitScript(
  (data) => {
    localStorage.setItem("tracebox.history.v1", JSON.stringify(data));
  },
  [py3, cpp],
);

const page = await ctx.newPage();
await page.goto(FRONTEND, { waitUntil: "networkidle" });
await page.waitForSelector(".monaco-editor .view-lines", { timeout: 20000 });
await page.waitForTimeout(800); // let Monaco finish painting

await page.screenshot({ path: `${OUT}/01-initial.png` });
console.log("captured 01-initial.png");

// Click the py3 entry → success result panel.
await page.locator(".history-list li").nth(0).locator(".history-item").click();
await page.waitForSelector(".result-explain.success", { timeout: 10000 });
await page.waitForTimeout(400);
await page.screenshot({ path: `${OUT}/02-success.png` });
console.log("captured 02-success.png");

// Click the cpp entry → build_failed result panel.
await page.locator(".history-list li").nth(1).locator(".history-item").click();
await page.waitForSelector(".result-explain.error", { timeout: 10000 });
await page.waitForTimeout(400);
await page.screenshot({ path: `${OUT}/03-build-failed.png` });
console.log("captured 03-build-failed.png");

await browser.close();
console.log("done");
