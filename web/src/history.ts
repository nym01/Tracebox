// Run history, persisted in browser localStorage (no backend). Each entry keeps
// enough to repopulate the editor and results panel when clicked.

import type { RunResponse } from "./api";

const STORAGE_KEY = "tracebox.history.v1";
const MAX_ENTRIES = 50;
const SNIPPET_LEN = 80;

export interface HistoryEntry {
  /** Locally-generated id for the list (the run_id may repeat on errors). */
  id: string;
  timestamp: number;
  language: string;
  /** Short single-line preview of the source. */
  snippet: string;
  /** Plain-language status label (e.g. "Ran", "Crashed"). */
  statusLabel: string;
  /** Raw API status, for re-deriving tone/explanation on reload. */
  status: string;
  runId: string;
  /** Full source + stdin so a click restores the exact run. */
  source: string;
  stdin: string;
  /** The full response, so the results panel can be rebuilt verbatim. */
  response: RunResponse;
}

function makeSnippet(source: string): string {
  const firstLine =
    source
      .split("\n")
      .map((l) => l.trim())
      .find((l) => l.length > 0) ?? "";
  return firstLine.length > SNIPPET_LEN
    ? firstLine.slice(0, SNIPPET_LEN) + "…"
    : firstLine || "(empty)";
}

export function loadHistory(): HistoryEntry[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as HistoryEntry[]) : [];
  } catch {
    return [];
  }
}

function persist(entries: HistoryEntry[]): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(entries));
  } catch {
    /* quota or disabled storage — history is best-effort */
  }
}

export interface NewRun {
  language: string;
  source: string;
  stdin: string;
  status: string;
  statusLabel: string;
  response: RunResponse;
}

/** Prepend a run to history (most recent first), capped at MAX_ENTRIES. */
export function addHistory(
  existing: HistoryEntry[],
  run: NewRun,
): HistoryEntry[] {
  const entry: HistoryEntry = {
    id:
      typeof crypto !== "undefined" && "randomUUID" in crypto
        ? crypto.randomUUID()
        : `${Date.now()}-${Math.random().toString(36).slice(2)}`,
    timestamp: Date.now(),
    language: run.language,
    snippet: makeSnippet(run.source),
    statusLabel: run.statusLabel,
    status: run.status,
    runId: run.response.run_id,
    source: run.source,
    stdin: run.stdin,
    response: run.response,
  };
  const next = [entry, ...existing].slice(0, MAX_ENTRIES);
  persist(next);
  return next;
}

export function clearHistory(): HistoryEntry[] {
  persist([]);
  return [];
}
