import type { Cell, QueryResult } from "../api";

function Td({ cell }: { cell: Cell }) {
  if (cell === null) return <td className="null">NULL</td>;
  if (typeof cell === "number") return <td className="num">{String(cell)}</td>;
  if (typeof cell === "object") return <td>{JSON.stringify(cell)}</td>;
  return <td>{String(cell)}</td>;
}

export function Results({ result }: { result: QueryResult | null }) {
  if (!result) return <div className="results" />;

  if (result.error) {
    return (
      <div className="results">
        <div className="banner err">{result.error}</div>
      </div>
    );
  }

  if (result.explain != null) {
    return (
      <div className="results">
        <pre className="plan">{result.explain}</pre>
      </div>
    );
  }

  return (
    <div className="results">
      {result.truncated && (
        <div className="banner note">
          results truncated to {result.count} rows (raise with --max-rows)
        </div>
      )}
      <table>
        <thead>
          <tr>
            {result.columns.map((c) => (
              <th key={c.name}>
                {c.name}
                <span className="ty">{c.type}</span>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {result.rows.map((row, i) => (
            <tr key={i}>
              {row.map((cell, j) => (
                <Td key={j} cell={cell} />
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
