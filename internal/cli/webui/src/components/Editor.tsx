import { forwardRef, type KeyboardEvent } from "react";
import type { QueryResult } from "../api";
import { resultToCSV, downloadCSV } from "../csv";

interface EditorProps {
  value: string;
  onChange: (v: string) => void;
  onRun: () => void;
  onExplain: () => void;
  running: boolean;
  result: QueryResult | null;
  status: string;
}

export const Editor = forwardRef<HTMLTextAreaElement, EditorProps>(
  function Editor(
    { value, onChange, onRun, onExplain, running, result, status },
    ref,
  ) {
    const canExport = !!result && !result.error && result.explain == null;

    const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
      if ((e.ctrlKey || e.metaKey) && e.key === "Enter") {
        e.preventDefault();
        onRun();
      }
    };

    return (
      <>
        <div className="editor">
          <textarea
            ref={ref}
            spellCheck={false}
            placeholder="SELECT * FROM ... LIMIT 10"
            value={value}
            onChange={(e) => onChange(e.target.value)}
            onKeyDown={onKeyDown}
          />
        </div>
        <div className="toolbar">
          <button onClick={onRun} disabled={running}>
            Run
          </button>
          <button className="ghost" onClick={onExplain} disabled={running}>
            Explain
          </button>
          <button
            className="ghost"
            disabled={!canExport}
            onClick={() => result && downloadCSV(resultToCSV(result))}
          >
            Export CSV
          </button>
          <span className="hint">
            &nbsp;<kbd>Ctrl/⌘</kbd>+<kbd>Enter</kbd> to run
          </span>
          <span className="status">{status}</span>
        </div>
      </>
    );
  },
);
