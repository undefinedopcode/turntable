import { forwardRef, useEffect, useImperativeHandle, useMemo, useRef } from "react";
import CodeMirror from "@uiw/react-codemirror";
import type { EditorView } from "@codemirror/view";
import { sql } from "@codemirror/lang-sql";
import { autocompletion, type Completion } from "@codemirror/autocomplete";
import { keymap } from "@codemirror/view";
import { Prec } from "@codemirror/state";
import { completionSource } from "../completions";

export interface EditorHandle {
  insert: (text: string) => void;
}

interface EditorProps {
  value: string;
  onChange: (v: string) => void;
  onRun: () => void;
  onExplain: () => void;
  running: boolean;
  status: string;
  completions: Completion[];
}

export const Editor = forwardRef<EditorHandle, EditorProps>(function Editor(
  { value, onChange, onRun, onExplain, running, status, completions },
  ref,
) {
  const viewRef = useRef<EditorView | null>(null);
  const onRunRef = useRef(onRun);
  onRunRef.current = onRun;

  // Push programmatic value changes (load from history/saved, source preview)
  // into the editor; the controlled `value` prop alone isn't reliably reflected.
  useEffect(() => {
    const view = viewRef.current;
    if (view && value !== view.state.doc.toString()) {
      view.dispatch({
        changes: { from: 0, to: view.state.doc.length, insert: value },
      });
    }
  }, [value]);

  useImperativeHandle(
    ref,
    () => ({
      insert: (text: string) => {
        const view = viewRef.current;
        if (!view) {
          onChange(value + text);
          return;
        }
        const { from, to } = view.state.selection.main;
        view.dispatch({
          changes: { from, to, insert: text },
          selection: { anchor: from + text.length },
        });
        view.focus();
      },
    }),
    [value, onChange],
  );

  const extensions = useMemo(
    () => [
      sql(),
      autocompletion({ override: [completionSource(completions)] }),
      // Ctrl/⌘+Enter runs the query (above the default completion keymap).
      Prec.highest(
        keymap.of([
          {
            key: "Mod-Enter",
            run: () => {
              onRunRef.current();
              return true;
            },
          },
        ]),
      ),
    ],
    [completions],
  );

  return (
    <>
      <div className="editor">
        <CodeMirror
          value={value}
          height="120px"
          theme="dark"
          placeholder="SELECT * FROM … LIMIT 10"
          basicSetup={{
            lineNumbers: false,
            foldGutter: false,
            highlightActiveLine: false,
            highlightActiveLineGutter: false,
          }}
          extensions={extensions}
          onChange={onChange}
          onCreateEditor={(view) => (viewRef.current = view)}
        />
      </div>
      <div className="toolbar">
        <button onClick={onRun} disabled={running}>
          Run
        </button>
        <button className="ghost" onClick={onExplain} disabled={running}>
          Explain
        </button>
        <span className="hint">
          &nbsp;<kbd>Ctrl/⌘</kbd>+<kbd>Enter</kbd> to run
        </span>
        <span className="status">{status}</span>
      </div>
    </>
  );
});
