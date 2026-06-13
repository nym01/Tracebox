// Configure @monaco-editor/react to use the locally-bundled `monaco-editor`
// package instead of loading it from a CDN at runtime. This makes the frontend
// work fully offline / air-gapped — no silent failure when the network is
// restricted.
//
// Rather than importing the whole `monaco-editor` package (which bundles ~80
// language grammars and the JSON/CSS/HTML/TS language services), we import the
// core editor API and register only the languages this app actually offers.
// They are all Monarch-tokenised "basic languages" — syntax highlighting needs
// no web worker — so the only worker we bundle is the core editor worker.

import * as monaco from "monaco-editor/esm/vs/editor/editor.api";

// Language grammars for the 7 supported languages. cpp.contribution registers
// BOTH "c" and "cpp"; systemverilog covers our Verilog mode.
import "monaco-editor/esm/vs/basic-languages/python/python.contribution";
import "monaco-editor/esm/vs/basic-languages/cpp/cpp.contribution";
import "monaco-editor/esm/vs/basic-languages/shell/shell.contribution";
import "monaco-editor/esm/vs/basic-languages/javascript/javascript.contribution";
import "monaco-editor/esm/vs/basic-languages/java/java.contribution";
import "monaco-editor/esm/vs/basic-languages/systemverilog/systemverilog.contribution";

import editorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import { loader } from "@monaco-editor/react";

self.MonacoEnvironment = {
  getWorker() {
    return new editorWorker();
  },
};

// Point the loader at the bundled monaco instance rather than the CDN.
loader.config({ monaco });
