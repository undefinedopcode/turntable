import { useCallback, useRef, useState } from "react";
import { runQuery, type QueryResult } from "./api";
import { Sidebar } from "./components/Sidebar";
import { Editor } from "./components/Editor";
import { Results } from "./components/Results";

export function App() {
  const [query, setQuery] = useState("");
  const [result, setResult] = useState<QueryResult | null>(null);
  const [status, setStatus] = useState("");
  const [running, setRunning] = useState(false);
  const [sourcesVersion, setSourcesVersion] = useState(0);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  const insertAtCursor = useCallback((text: string) => {
    const el = textareaRef.current;
    if (!el) {
      setQuery((q) => q + text);
      return;
    }
    const { selectionStart: start, selectionEnd: end, value } = el;
    const next = value.slice(0, start) + text + value.slice(end);
    setQuery(next);
    // Restore the caret just after the inserted text on the next tick.
    requestAnimationFrame(() => {
      el.focus();
      el.selectionStart = el.selectionEnd = start + text.length;
    });
  }, []);

  const run = useCallback(
    async (explain: boolean) => {
      const q = query.trim();
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

  return (
    <div className="app">
      <header>
        <span className="brand">turntable</span>
        <span className="tag">query anything with SQL</span>
      </header>
      <main>
        <Sidebar
          key={sourcesVersion}
          onInsert={insertAtCursor}
          onSourceAdded={() => setSourcesVersion((v) => v + 1)}
        />
        <section className="work">
          <Editor
            ref={textareaRef}
            value={query}
            onChange={setQuery}
            onRun={() => run(false)}
            onExplain={() => run(true)}
            running={running}
            result={result}
            status={status}
          />
          <Results result={result} />
        </section>
      </main>
    </div>
  );
}
