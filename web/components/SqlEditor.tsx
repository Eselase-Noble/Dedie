"use client";

import { MySQL, PostgreSQL, sql } from "@codemirror/lang-sql";
import { Prec } from "@codemirror/state";
import { EditorView, keymap } from "@codemirror/view";
import CodeMirror from "@uiw/react-codemirror";
import { useMemo, useRef, type MutableRefObject } from "react";

/**
 * SQL editor that fills its container.
 *
 * Ctrl+Enter (Cmd+Enter on macOS) runs the statement. When text is selected,
 * only the selection runs, which is how one statement out of several is
 * executed. `selection` gives the toolbar's Run button the same behaviour.
 */
export default function SqlEditor({ value, onChange, onRun, kind, schema, selection }: {
  value: string;
  onChange: (value: string) => void;
  onRun: (selected: string) => void;
  /** Database kind of the connection, which selects the SQL dialect. */
  kind?: string;
  /** Table name to column names, offered as completions. */
  schema?: Record<string, string[]>;
  /** Filled with a function returning the current selection. */
  selection?: MutableRefObject<() => string>;
}) {
  // The keymap is created once per dialect; the ref keeps it calling the
  // latest handler.
  const runRef = useRef(onRun);
  runRef.current = onRun;

  const selected = (view: EditorView) => {
    const range = view.state.selection.main;
    return view.state.sliceDoc(range.from, range.to);
  };

  const extensions = useMemo(
    () => [
      sql({ dialect: kind === "mysql" ? MySQL : PostgreSQL, schema, upperCaseKeywords: true }),
      // Long statements wrap instead of running off to the right.
      EditorView.lineWrapping,
      Prec.highest(keymap.of([{ key: "Mod-Enter", run: (view) => { runRef.current(selected(view)); return true; } }])),
    ],
    [kind, schema],
  );

  return (
    <div className="editor">
      <CodeMirror
        value={value}
        onChange={onChange}
        onCreateEditor={(view) => { if (selection) selection.current = () => selected(view); }}
        extensions={extensions}
        theme="light"
        height="100%"
        basicSetup={{ foldGutter: false, highlightActiveLine: true }}
        placeholder="Write SQL here. Ctrl/Cmd + Enter runs it; select part of it to run only that part."
        aria-label="SQL editor"
      />
    </div>
  );
}
