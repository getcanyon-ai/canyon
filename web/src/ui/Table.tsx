import type { ReactNode } from "react";

export const HEAD = "border-b border-border bg-surface-2 px-2.5 py-2.5 text-left text-sm font-semibold text-text-faint";
// A cell whose words may wrap; CELL keeps its line.
export const WRAP_CELL = "border-b border-border px-2.5 py-1.5 align-middle";
export const CELL = `${WRAP_CELL} whitespace-nowrap`;

// TableWrap is not optional: the fleet table is 1140px minimum and the detail tables 520px, so an
// unwrapped table breaks the page sideways at 820px. The body never scrolls sideways; tables do.
export function TableWrap({ minWidth, children }: { minWidth: number; children: ReactNode }) {
  return (
    <div className="mt-2.5 overflow-x-auto rounded-xl border border-border">
      <div style={{ minWidth }}>{children}</div>
    </div>
  );
}

interface TableProps<Row> {
  columns: { key: string; label: string; className?: string }[];
  rows: Row[];
  renderRow: (row: Row) => ReactNode;
  empty?: ReactNode;
}

export function Table<Row>({ columns, rows, renderRow, empty = "No rows" }: TableProps<Row>) {
  return (
    <table className="w-full border-collapse bg-surface text-base">
      <thead>
        <tr>{columns.map((c) => <th key={c.key} className={`${HEAD} ${c.className ?? ""}`}>{c.label}</th>)}</tr>
      </thead>
      <tbody>
        {rows.length === 0
          ? <tr><td className="px-3 py-4 text-text-faint" colSpan={columns.length}>{empty}</td></tr>
          : rows.map(renderRow)}
      </tbody>
    </table>
  );
}
