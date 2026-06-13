import type { HistoryEntry } from "../history";
import { statusTone } from "../explain";
import { languageById } from "../languages";

interface Props {
  entries: HistoryEntry[];
  activeId: string | null;
  onSelect: (entry: HistoryEntry) => void;
  onClear: () => void;
}

function relativeTime(ts: number): string {
  const secs = Math.round((Date.now() - ts) / 1000);
  if (secs < 60) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  const days = Math.round(hrs / 24);
  return `${days}d ago`;
}

export default function HistoryPanel({
  entries,
  activeId,
  onSelect,
  onClear,
}: Props) {
  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <h2>History</h2>
        <button
          className="text-btn"
          onClick={onClear}
          disabled={entries.length === 0}
          title="Clear all history"
        >
          Clear
        </button>
      </div>

      {entries.length === 0 ? (
        <div className="history-empty">
          No runs yet. Your past runs will appear here.
        </div>
      ) : (
        <ul className="history-list">
          {entries.map((e) => (
            <li key={e.id}>
              <button
                className={`history-item${e.id === activeId ? " active" : ""}`}
                onClick={() => onSelect(e)}
                title={`${e.statusLabel} · run ${e.runId}`}
              >
                <div className="history-row">
                  <span className="history-lang">
                    <span className={`dot ${statusTone(e.status)}`} />{" "}
                    {languageById(e.language).name}
                  </span>
                  <span className="history-time">
                    {relativeTime(e.timestamp)}
                  </span>
                </div>
                <span className="history-snippet">{e.snippet}</span>
                <span className="history-time">{e.statusLabel}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </aside>
  );
}
