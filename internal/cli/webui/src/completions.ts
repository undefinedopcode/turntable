import type {
  Completion,
  CompletionContext,
  CompletionResult,
} from "@codemirror/autocomplete";
import { getFunctions, getSchema, listSources } from "./api";

// buildCompletions fetches the live dialect functions, sources, and columns and
// turns them into CodeMirror completion entries. Best-effort: any failed fetch
// simply contributes nothing.
export async function buildCompletions(): Promise<Completion[]> {
  const out: Completion[] = [];

  try {
    const fns = await getFunctions();
    for (const k of fns.keywords) out.push({ label: k, type: "keyword" });
    for (const f of fns.scalar)
      out.push({ label: f, type: "function", detail: "fn" });
    for (const a of fns.aggregate)
      out.push({ label: a, type: "function", detail: "agg" });
  } catch {
    /* ignore */
  }

  try {
    const sources = await listSources();
    for (const s of sources)
      out.push({ label: s.name, type: "class", detail: s.connector });
    const cols = new Map<string, string>();
    await Promise.all(
      sources.map(async (s) => {
        try {
          const sc = await getSchema(s.name);
          for (const c of sc.columns ?? [])
            if (!cols.has(c.name)) cols.set(c.name, c.type);
        } catch {
          /* ignore one source's schema */
        }
      }),
    );
    for (const [name, type] of cols)
      out.push({ label: name, type: "property", detail: type });
  } catch {
    /* ignore */
  }

  return out;
}

// completionSource returns a CodeMirror completion function over a fixed item
// list, matching the identifier fragment before the cursor.
export function completionSource(items: Completion[]) {
  return (ctx: CompletionContext): CompletionResult | null => {
    const word = ctx.matchBefore(/[\w$]+/);
    if (!word || (word.from === word.to && !ctx.explicit)) return null;
    return { from: word.from, options: items, validFor: /^[\w$]*$/ };
  };
}
