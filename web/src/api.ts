// Thin client over the Tracebox HTTP API. Mirrors the request/response shapes in
// internal/api (handlers.go, validate.go) — kept deliberately minimal: a single
// source file, a single test case carrying optional stdin and no expected_stdout,
// since this UI shows what the program did rather than grading it.

export const API_BASE_URL = (
  import.meta.env.VITE_TRACEBOX_API_URL ?? "http://localhost:8080"
)
  .toString()
  .replace(/\/+$/, "");

export interface RunRequest {
  language: string;
  source: string;
  source_filename?: string;
  artifact_filename?: string;
  tests: { stdin?: string; expected_stdout?: string }[];
}

export interface BuildResult {
  status: string;
  stdout: string;
  stderr: string;
  duration_ms: number;
}

export interface TestResult {
  status: string;
  stdout: string;
  stderr: string;
  duration_ms: number;
  memory_peak_kb: number;
}

export interface RunResponse {
  run_id: string;
  status: string;
  build?: BuildResult;
  tests: TestResult[];
}

interface ApiErrorBody {
  error?: { code?: string; message?: string };
}

/** Error thrown for non-2xx API responses, carrying the server's code/message. */
export class ApiError extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.code = code;
  }
}

// javaClassRe extracts the public top-level class name, mirroring tracebox-mcp:
// javac requires the source file (and run target) to be named after the class.
const javaClassRe =
  /^\s*public\s+(?:final\s+|abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)/m;

function buildRequest(
  language: string,
  source: string,
  stdin: string,
): RunRequest {
  const req: RunRequest = {
    language,
    source,
    tests: [{ stdin, expected_stdout: "" }],
  };
  if (language === "java") {
    const match = source.match(javaClassRe);
    const cls = match ? match[1] : "Main";
    req.source_filename = `${cls}.java`;
    req.artifact_filename = cls;
  }
  return req;
}

/** POST the source to /run and return the parsed response. */
export async function runCode(
  language: string,
  source: string,
  stdin: string,
  signal?: AbortSignal,
): Promise<RunResponse> {
  const body = buildRequest(language, source, stdin);

  let resp: Response;
  try {
    resp = await fetch(`${API_BASE_URL}/run`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal,
    });
  } catch (err) {
    throw new ApiError(
      "network_error",
      `Could not reach the Tracebox API at ${API_BASE_URL}. Is the server running?`,
    );
  }

  const text = await resp.text();

  if (!resp.ok) {
    let parsed: ApiErrorBody | null = null;
    try {
      parsed = JSON.parse(text) as ApiErrorBody;
    } catch {
      /* fall through to a generic message */
    }
    if (parsed?.error?.message) {
      throw new ApiError(
        parsed.error.code ?? "api_error",
        parsed.error.message,
      );
    }
    throw new ApiError("api_error", `API returned status ${resp.status}.`);
  }

  return JSON.parse(text) as RunResponse;
}
