import { useCallback, useEffect, useRef, useState } from "react";
import type { Completion } from "@codemirror/autocomplete";
import { runQuery, type QueryResult } from "./api";
import { Sidebar } from "./components/Sidebar";
import { Editor, type EditorHandle } from "./components/Editor";
import { Results } from "./components/Results";
import { loadLastQuery, pushHistory, saveLastQuery } from "./storage";
import { buildCompletions } from "./completions";

export function App() {
  const [query, setQuery] = useState(() => loadLastQuery());
  const [result, setResult] = useState<QueryResult | null>(null);
  const [status, setStatus] = useState("");
  const [running, setRunning] = useState(false);
  const [sourcesVersion, setSourcesVersion] = useState(0);
  const [historyVersion, setHistoryVersion] = useState(0);
  const [completions, setCompletions] = useState<Completion[]>([]);
  const editorRef = useRef<EditorHandle>(null);

  // Persist the editor content and (re)load autocompletion when sources change.
  useEffect(() => saveLastQuery(query), [query]);
  useEffect(() => {
    buildCompletions().then(setCompletions);
  }, [sourcesVersion]);

  const run = useCallback(
    async (explain: boolean, override?: string) => {
      const q = (override ?? query).trim();
      if (!q) return;
      setRunning(true);
      setStatus("running…");
      try {
        const data = await runQuery(q, explain);
        setResult(data);
        if (data.error) {
          setStatus(data.elapsed_ms != null ? `${data.elapsed_ms} ms` : "");
        } else if (data.explain != null) {
          setStatus(`${data.elapsed_ms} ms`);
        } else {
          setStatus(
            `${data.count} row${data.count === 1 ? "" : "s"} · ${data.elapsed_ms} ms`,
          );
          pushHistory(q);
          setHistoryVersion((v) => v + 1);
        }
      } catch (e) {
        setResult({
          columns: [],
          rows: [],
          count: 0,
          elapsed_ms: 0,
          error: String(e),
        });
        setStatus("");
      } finally {
        setRunning(false);
      }
    },
    [query],
  );

  const runNow = useCallback(
    (q: string) => {
      setQuery(q);
      run(false, q);
    },
    [run],
  );

  return (
    <div className="app">
      <header>
        <span className="brand">turntable</span>
        <span className="tag">query anything with SQL</span>
      </header>
      <main>
        <Sidebar
          onInsert={(t) => editorRef.current?.insert(t)}
          onSourceAdded={() => setSourcesVersion((v) => v + 1)}
          onLoadQuery={setQuery}
          onRunQuery={runNow}
          currentQuery={query}
          historyVersion={historyVersion}
        />
        <section className="work">
          <Editor
            ref={editorRef}
            value={query}
            onChange={setQuery}
            onRun={() => run(false)}
            onExplain={() => run(true)}
            running={running}
            status={status}
            completions={completions}
          />
          <Results result={result} />
        </section>
      </main>
    </div>
  );
}
