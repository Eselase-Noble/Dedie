"use client";

import { PostgreSQL, sql } from "@codemirror/lang-sql";
import { Prec } from "@codemirror/state";
import { keymap } from "@codemirror/view";
import CodeMirror from "@uiw/react-codemirror";
import { useMemo, useRef } from "react";

/** SQL editor. Ctrl+Enter (Cmd+Enter on macOS) runs the statement. */
export default function SqlEditor({ value, onChange, onRun }: {
  value: string; onChange: (value: string) => void; onRun: () => void;
}) {
  // The keymap is created once; the ref keeps it calling the latest handler.
  const runRef = useRef(onRun);
  runRef.current = onRun;

  const extensions = useMemo(
    () => [
      sql({ dialect: PostgreSQL, upperCaseKeywords: true }),
      Prec.highest(keymap.of([{ key: "Mod-Enter", run: () => { runRef.current(); return true; } }])),
    ],
    [],
  );

  return (
    <div className="editor">
      <CodeMirror
        value={value}
        onChange={onChange}
        extensions={extensions}
        theme="light"
        height="220px"
        basicSetup={{ foldGutter: false, highlightActiveLine: false }}
        aria-label="SQL editor"
      />
    </div>
  );
}
