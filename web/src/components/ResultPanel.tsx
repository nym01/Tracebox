import type { RunResponse } from "../api";
import { explain, statusTone } from "../explain";
import type { LanguageDef } from "../languages";

interface Props {
  response: RunResponse;
  lang: LanguageDef;
}

const TONE_ICON: Record<string, string> = {
  success: "✓",
  error: "✕",
  warning: "⏱",
  info: "ℹ",
};

function byteLen(s: string): string {
  const n = new TextEncoder().encode(s).length;
  return n === 1 ? "1 byte" : `${n} bytes`;
}

/** A collapsible, scrollable output stream block. */
function Stream({
  title,
  content,
  variant,
  open,
}: {
  title: string;
  content: string;
  variant?: "stderr";
  open: boolean;
}) {
  const empty = content.length === 0;
  return (
    <details className="output" open={open && !empty}>
      <summary>
        {title}
        <span className="count">{empty ? "empty" : byteLen(content)}</span>
      </summary>
      {empty ? (
        <pre className="stream empty">(no output)</pre>
      ) : (
        <pre className={`stream${variant ? " " + variant : ""}`}>{content}</pre>
      )}
    </details>
  );
}

export default function ResultPanel({ response, lang }: Props) {
  const ex = explain(response, lang);
  const tone = ex.tone;
  const test = response.tests[0];
  const build = response.build;

  const isBuildFailed = response.status === "build_failed";
  const ran = tone === "success";

  const compileOutput = build
    ? [build.stdout, build.stderr].filter(Boolean).join("\n")
    : "";

  const durationMs = (test?.duration_ms ?? 0) + (build?.duration_ms ?? 0);
  const memoryKB = test?.memory_peak_kb ?? 0;

  return (
    <div className="result">
      <div className={`result-explain ${tone}`}>
        <span className="icon">{TONE_ICON[tone] ?? "•"}</span>
        <div>
          <h2>{ex.title}</h2>
          <p>{ex.detail}</p>
        </div>
      </div>

      <div className="result-meta">
        <span>
          <span className={`badge ${statusTone(response.status)}`}>
            {ex.label}
          </span>
        </span>
        <span>
          run_id <code>{response.run_id}</code>
        </span>
        <span>
          duration <code>{durationMs} ms</code>
        </span>
        {memoryKB > 0 && (
          <span>
            peak memory <code>{memoryKB.toLocaleString()} KB</code>
          </span>
        )}
        <span>
          raw status <code>{response.status}</code>
        </span>
      </div>

      <div className="outputs">
        {build && (
          <Stream
            title="Compiler output"
            content={compileOutput}
            variant="stderr"
            open={isBuildFailed}
          />
        )}
        <Stream
          title="Standard output (stdout)"
          content={test?.stdout ?? ""}
          open={ran}
        />
        <Stream
          title="Standard error (stderr)"
          content={test?.stderr ?? ""}
          variant="stderr"
          open={!ran && !isBuildFailed}
        />
      </div>
    </div>
  );
}
