// End-to-end test of the Tracebox frontend against the real API.
// Drives the dev server (http://localhost:5173) with headless Chromium and
// exercises: (a) a successful py3 run, (b) a cpp build_failed, (c) history
// record + restore. Captures console errors and failed network requests.

import { chromium } from "playwright";

const FRONTEND = process.env.FRONTEND_URL ?? "http://localhost:5173";
const results = [];
const consoleErrors = [];
const failedRequests = [];

function log(step, ok, detail) {
  results.push({ step, ok, detail });
  console.log(`${ok ? "PASS" : "FAIL"} — ${step}${detail ? ": " + detail : ""}`);
}

// Replace the Monaco editor's content deterministically. insertText avoids
// Monaco's per-keystroke auto-closing of quotes/brackets.
async function setEditor(page, code) {
  await page.click(".monaco-editor .view-lines");
  await page.keyboard.press("Control+A");
  await page.keyboard.press("Delete");
  await page.keyboard.insertText(code);
}

async function editorText(page) {
  return (await page.locator(".monaco-editor .view-lines").innerText()).trim();
}

const browser = await chromium.launch();
const page = await browser.newPage();

page.on("console", (msg) => {
  if (msg.type() === "error") consoleErrors.push(msg.text());
});
page.on("requestfailed", (req) => {
  failedRequests.push(`${req.method()} ${req.url()} — ${req.failure()?.errorText}`);
});
page.on("response", (resp) => {
  if (resp.url().includes("/run") && !resp.ok()) {
    failedRequests.push(`HTTP ${resp.status()} ${resp.url()}`);
  }
});

try {
  await page.goto(FRONTEND, { waitUntil: "networkidle" });
  await page.waitForSelector(".monaco-editor .view-lines", { timeout: 20000 });
  log("Frontend loads and Monaco editor mounts", true);

  // ---- (a) py3 success ----
  await page.selectOption("#lang", "py3");
  await setEditor(page, 'print("hello from frontend")');
  await page.click(".run-btn");
  await page.waitForSelector(".result-explain.success", { timeout: 30000 });

  const successTitle = (await page.locator(".result-explain h2").innerText()).trim();
  const stdout = (await page.locator("pre.stream").first().innerText()).trim();
  const metaText = await page.locator(".result-meta").innerText();
  const runIdMatch = metaText.match(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/);

  log(
    "(a) py3: 'ran successfully' explanation",
    /ran successfully/i.test(successTitle),
    successTitle,
  );
  log(
    "(a) py3: stdout shows program output",
    stdout.includes("hello from frontend"),
    JSON.stringify(stdout),
  );
  log(
    "(a) py3: run_id present",
    !!runIdMatch,
    runIdMatch ? runIdMatch[0] : "none",
  );

  // ---- (b) cpp build_failed ----
  await page.selectOption("#lang", "cpp");
  // Invalid C++: missing semicolon + undeclared, guaranteed compile error.
  await setEditor(
    page,
    "int main() {\n  this is not valid c++\n  return 0\n}",
  );
  await page.click(".run-btn");
  await page.waitForSelector(".result-explain.error", { timeout: 30000 });

  const failTitle = (await page.locator(".result-explain h2").innerText()).trim();
  // The compiler-output block auto-opens for build_failed; read its <pre>.
  const compilerBlock = page.locator("details.output", { hasText: "Compiler output" });
  const compilerOpen = await compilerBlock.evaluate((el) => el.open).catch(() => false);
  const compilerText = (await compilerBlock.locator("pre.stream").innerText().catch(() => "")).trim();

  log(
    "(b) cpp: 'failed to compile' explanation",
    /failed to compile/i.test(failTitle),
    failTitle,
  );
  log(
    "(b) cpp: compiler output shown and non-empty",
    compilerOpen && compilerText.length > 0,
    JSON.stringify(compilerText.slice(0, 160)),
  );

  // ---- (c) history records both, click restores ----
  const items = page.locator(".history-list li");
  const count = await items.count();
  log("(c) history records both runs", count === 2, `${count} entries`);

  // Newest first → [cpp(b), py3(a)]. Click the py3 entry (index 1) to restore.
  await items.nth(1).locator(".history-item").click();
  await page.waitForSelector(".result-explain.success", { timeout: 10000 });

  const restoredLang = await page.locator("#lang").inputValue();
  const restoredCode = await editorText(page);
  const restoredTitle = (await page.locator(".result-explain h2").innerText()).trim();

  log(
    "(c) clicking py3 history entry restores language",
    restoredLang === "py3",
    `lang=${restoredLang}`,
  );
  log(
    "(c) clicking py3 history entry restores code",
    restoredCode.includes("hello from frontend"),
    JSON.stringify(restoredCode),
  );
  log(
    "(c) clicking py3 history entry restores its result",
    /ran successfully/i.test(restoredTitle),
    restoredTitle,
  );
} catch (err) {
  log("UNEXPECTED ERROR", false, err.message);
} finally {
  console.log("\n--- console errors ---");
  console.log(consoleErrors.length ? consoleErrors.join("\n") : "(none)");
  console.log("\n--- failed requests / non-2xx /run ---");
  console.log(failedRequests.length ? failedRequests.join("\n") : "(none)");
  const failed = results.filter((r) => !r.ok);
  console.log(`\n=== ${results.length - failed.length}/${results.length} checks passed ===`);
  await browser.close();
  process.exit(failed.length ? 1 : 0);
}
