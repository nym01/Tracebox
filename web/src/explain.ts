// Rule-based, plain-English explanation of a run result. No LLM: every status
// maps to a fixed human explanation. The mapping of "what counts as ran vs. a
// real failure" follows tracebox-mcp's reportedStatus() — because this UI never
// supplies an expected output, the API's comparison verdicts (accepted /
// wrong_output / output_whitespace_mismatch) are NOT pass/fail signals. They all
// mean "your code ran"; only genuine execution failures are framed as failures.

import type { RunResponse } from "./api";
import { formatMemoryKB, type LanguageDef } from "./languages";

export type ExplanationTone = "success" | "error" | "warning" | "info";

export interface Explanation {
  tone: ExplanationTone;
  /** Short plain-language label, used in badges and the history list. */
  label: string;
  /** One-line headline. */
  title: string;
  /** A sentence or two of plain-English detail. */
  detail: string;
}

// Statuses that mean "the program executed", regardless of output comparison.
const RAN_STATUSES = new Set([
  "accepted",
  "wrong_output",
  "output_whitespace_mismatch",
]);

/**
 * Maps a top-level run status to a short, human-friendly label. Used for the
 * results badge and the history sidebar so both speak the same language.
 */
export function statusLabel(status: string): string {
  if (RAN_STATUSES.has(status)) return "Ran";
  switch (status) {
    case "runtime_error":
      return "Crashed";
    case "time_exceeded":
      return "Timed out";
    case "memory_exceeded":
      return "Out of memory";
    case "build_failed":
      return "Build failed";
    case "internal_error":
      return "Sandbox error";
    case "not_executed":
      return "Not run";
    default:
      return status.replace(/_/g, " ");
  }
}

/** The tone (colour family) for a status, for badges and panels. */
export function statusTone(status: string): ExplanationTone {
  if (RAN_STATUSES.has(status)) return "success";
  switch (status) {
    case "time_exceeded":
    case "memory_exceeded":
      return "warning";
    case "internal_error":
    case "not_executed":
      return "info";
    default:
      return "error";
  }
}

/**
 * Builds the full plain-English explanation for a completed run.
 * `lang` supplies the configured time/memory limits so the explanation can name
 * them (the API response itself does not echo the limits back).
 */
export function explain(resp: RunResponse, lang: LanguageDef): Explanation {
  const status = resp.status;
  const test = resp.tests[0];

  if (RAN_STATUSES.has(status)) {
    return {
      tone: "success",
      label: "Ran",
      title: "Your code ran successfully",
      detail:
        "The program executed to completion inside the sandbox. Its output is shown below — this view doesn't check it against any expected answer, so a clean run just means it finished without errors.",
    };
  }

  switch (status) {
    case "runtime_error":
      return {
        tone: "error",
        label: "Crashed",
        title: "Your code crashed or was stopped by the sandbox",
        detail:
          "The program exited with an error (a non-zero exit code) or was terminated by the sandbox. Check the standard error output below for the cause — an exception, a failed assertion, or a blocked operation.",
      };

    case "time_exceeded":
      return {
        tone: "warning",
        label: "Timed out",
        title: "Your code took too long and was stopped",
        detail: `The program ran past the ${lang.wallTimeS}-second time limit for ${lang.name} and was stopped by the sandbox. This usually points to an infinite loop or work that's too slow to finish in time.`,
      };

    case "memory_exceeded":
      return {
        tone: "warning",
        label: "Out of memory",
        title: "Your code used too much memory and was stopped",
        detail: `The program exceeded the ${formatMemoryKB(
          lang.memoryKB,
        )} memory limit for ${lang.name} and was killed by the sandbox. Look for large allocations, unbounded data structures, or runaway recursion.`,
      };

    case "build_failed":
      return {
        tone: "error",
        label: "Build failed",
        title: "Your code failed to compile",
        detail:
          "The compiler rejected your code before it could run. The compiler output below shows the errors — fix those and run again.",
      };

    case "internal_error":
      return {
        tone: "info",
        label: "Sandbox error",
        title: "The sandbox hit an internal error",
        detail:
          "Something went wrong inside Tracebox itself, not in your code. This isn't a problem with what you wrote — try running again.",
      };

    case "not_executed":
      return {
        tone: "info",
        label: "Not run",
        title: "Your code was not run",
        detail:
          "Execution was skipped, usually because an earlier phase (such as compilation) did not succeed.",
      };

    default: {
      const detail = test?.stderr
        ? "See the standard error output below for details."
        : "See the raw output below for details.";
      return {
        tone: "error",
        label: statusLabel(status),
        title: `The run finished with status "${status.replace(/_/g, " ")}"`,
        detail,
      };
    }
  }
}
