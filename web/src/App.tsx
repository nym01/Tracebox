import { useMemo, useRef, useState } from "react";
import CodeEditor from "./components/CodeEditor";
import ResultPanel from "./components/ResultPanel";
import HistoryPanel from "./components/HistoryPanel";
import { ApiError, API_BASE_URL, runCode, type RunResponse } from "./api";
import { DEFAULT_LANGUAGE, LANGUAGES, languageById } from "./languages";
import { statusLabel } from "./explain";
import {
  addHistory,
  clearHistory,
  loadHistory,
  type HistoryEntry,
} from "./history";

// Initial source per language, seeded from each language's starter snippet.
function initialSources(): Record<string, string> {
  const out: Record<string, string> = {};
  for (const l of LANGUAGES) out[l.id] = l.starter;
  return out;
}

export default function App() {
  const [langId, setLangId] = useState(DEFAULT_LANGUAGE.id);
  const [sources, setSources] = useState<Record<string, string>>(
    initialSources,
  );
  const [stdin, setStdin] = useState("");
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<RunResponse | null>(null);
  // Language a displayed result was produced with — its limits drive the
  // explanation, so it must track the result, not the current selector.
  const [resultLangId, setResultLangId] = useState(DEFAULT_LANGUAGE.id);
  const [error, setError] = useState<string | null>(null);
  const [history, setHistory] = useState<HistoryEntry[]>(loadHistory);
  const [activeHistoryId, setActiveHistoryId] = useState<string | null>(null);

  const abortRef = useRef<AbortController | null>(null);

  const lang = languageById(langId);
  const source = sources[langId] ?? "";
  const resultLang = useMemo(
    () => languageById(resultLangId),
    [resultLangId],
  );

  function setSource(value: string) {
    setSources((prev) => ({ ...prev, [langId]: value }));
  }

  async function handleRun() {
    if (running || source.trim().length === 0) return;
    setRunning(true);
    setError(null);
    setActiveHistoryId(null);

    const controller = new AbortController();
    abortRef.current = controller;

    try {
      const resp = await runCode(langId, source, stdin, controller.signal);
      setResult(resp);
      setResultLangId(langId);
      const label = statusLabel(resp.status);
      setHistory((prev) =>
        addHistory(prev, {
          language: langId,
          source,
          stdin,
          status: resp.status,
          statusLabel: label,
          response: resp,
        }),
      );
    } catch (err) {
      const message =
        err instanceof ApiError
          ? err.message
          : "Unexpected error while running your code.";
      setError(message);
      setResult(null);
    } finally {
      setRunning(false);
      abortRef.current = null;
    }
  }

  function handleSelectHistory(entry: HistoryEntry) {
    setLangId(entry.language);
    setSources((prev) => ({ ...prev, [entry.language]: entry.source }));
    setStdin(entry.stdin);
    setResult(entry.response);
    setResultLangId(entry.language);
    setError(null);
    setActiveHistoryId(entry.id);
  }

  function handleClearHistory() {
    setHistory(clearHistory());
    setActiveHistoryId(null);
  }

  return (
    <div className="app">
      <header className="app-header">
        <h1>Tracebox</h1>
        <span className="tagline">
          Write code, run it in the sandbox, see what happened.
        </span>
        <span className="api-url" title="API base URL (VITE_TRACEBOX_API_URL)">
          {API_BASE_URL}
        </span>
      </header>

      <div className="app-body">
        <main className="main-col">
          <div className="toolbar">
            <div className="field">
              <label htmlFor="lang">Language</label>
              <select
                id="lang"
                value={langId}
                onChange={(e) => setLangId(e.target.value)}
              >
                {LANGUAGES.map((l) => (
                  <option key={l.id} value={l.id}>
                    {l.name}
                  </option>
                ))}
              </select>
            </div>

            <button
              className="run-btn"
              onClick={handleRun}
              disabled={running || source.trim().length === 0}
            >
              {running ? (
                <>
                  <span className="spinner" /> Running…
                </>
              ) : (
                <>► Run</>
              )}
            </button>
          </div>

          <div>
            <span className="panel-label">Code</span>
            <CodeEditor
              language={lang.monaco}
              value={source}
              onChange={setSource}
            />
          </div>

          <div>
            <label className="panel-label" htmlFor="stdin">
              Standard input (stdin) — optional
            </label>
            <textarea
              id="stdin"
              className="stdin"
              value={stdin}
              placeholder="Text fed to your program's standard input…"
              onChange={(e) => setStdin(e.target.value)}
              spellCheck={false}
            />
          </div>

          {error && <div className="error-banner">{error}</div>}

          {result ? (
            <ResultPanel response={result} lang={resultLang} />
          ) : (
            !error && (
              <div className="placeholder">
                Run your code to see the output and a plain-English explanation
                of what happened.
              </div>
            )
          )}
        </main>

        <HistoryPanel
          entries={history}
          activeId={activeHistoryId}
          onSelect={handleSelectHistory}
          onClear={handleClearHistory}
        />
      </div>
    </div>
  );
}
