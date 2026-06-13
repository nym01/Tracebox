// Language definitions mirror configs/languages.yaml in the Go server. Each entry
// pairs a Tracebox language id with its display name, a Monaco editor language
// mode, the run-phase wall-time / memory limits (so the UI can mention them in
// "took too long" / "used too much memory" explanations), and a starter snippet.

export interface LanguageDef {
  id: string;
  name: string;
  /** Monaco editor language mode. */
  monaco: string;
  /** Run-phase wall-time limit, in seconds (from languages.yaml). */
  wallTimeS: number;
  /** Run-phase memory limit, in KiB (from languages.yaml). */
  memoryKB: number;
  /** Default file extension shown for context. */
  starter: string;
}

export const LANGUAGES: LanguageDef[] = [
  {
    id: "py3",
    name: "Python 3",
    monaco: "python",
    wallTimeS: 9,
    memoryKB: 102400,
    starter: 'print("Hello from Tracebox")\n',
  },
  {
    id: "cpp",
    name: "C++",
    monaco: "cpp",
    wallTimeS: 3,
    memoryKB: 524288,
    starter:
      '#include <iostream>\n\nint main() {\n    std::cout << "Hello from Tracebox\\n";\n    return 0;\n}\n',
  },
  {
    id: "c",
    name: "C",
    monaco: "c",
    wallTimeS: 3,
    memoryKB: 524288,
    starter:
      '#include <stdio.h>\n\nint main(void) {\n    printf("Hello from Tracebox\\n");\n    return 0;\n}\n',
  },
  {
    id: "bash",
    name: "Bash",
    monaco: "shell",
    wallTimeS: 5,
    memoryKB: 65536,
    starter: 'echo "Hello from Tracebox"\n',
  },
  {
    id: "js",
    name: "JavaScript",
    monaco: "javascript",
    wallTimeS: 5,
    memoryKB: 262144,
    starter: 'console.log("Hello from Tracebox");\n',
  },
  {
    id: "java",
    name: "Java",
    monaco: "java",
    wallTimeS: 6,
    memoryKB: 524288,
    starter:
      'public class Main {\n    public static void main(String[] args) {\n        System.out.println("Hello from Tracebox");\n    }\n}\n',
  },
  {
    id: "verilog",
    name: "Verilog",
    // Monaco ships no dedicated Verilog grammar; systemverilog is the closest fit.
    monaco: "systemverilog",
    wallTimeS: 5,
    memoryKB: 131072,
    starter:
      'module main;\n  initial begin\n    $display("Hello from Tracebox");\n    $finish;\n  end\nendmodule\n',
  },
];

export const DEFAULT_LANGUAGE = LANGUAGES[0];

export function languageById(id: string): LanguageDef {
  return LANGUAGES.find((l) => l.id === id) ?? DEFAULT_LANGUAGE;
}

/** Human-friendly memory size from a KiB value (e.g. 524288 -> "512 MB"). */
export function formatMemoryKB(kb: number): string {
  if (kb >= 1024 && kb % 1024 === 0) {
    return `${kb / 1024} MB`;
  }
  return `${kb} KB`;
}
