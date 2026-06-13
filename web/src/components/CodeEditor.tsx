import Editor from "@monaco-editor/react";

interface Props {
  language: string; // Monaco language mode
  value: string;
  onChange: (value: string) => void;
}

/** Monaco code editor wrapper with a dark theme matching the app. */
export default function CodeEditor({ language, value, onChange }: Props) {
  return (
    <div className="editor-shell">
      <Editor
        height="380px"
        language={language}
        theme="vs-dark"
        value={value}
        onChange={(v) => onChange(v ?? "")}
        options={{
          fontSize: 13,
          fontFamily:
            'ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
          minimap: { enabled: false },
          scrollBeyondLastLine: false,
          smoothScrolling: true,
          automaticLayout: true,
          tabSize: 2,
          padding: { top: 12, bottom: 12 },
          renderLineHighlight: "line",
          scrollbar: { verticalScrollbarSize: 10, horizontalScrollbarSize: 10 },
        }}
      />
    </div>
  );
}
