/**
 * Table — a semantic, responsive data table. Renders a real <table> with a
 * <caption> (visually hidden by default but read by screen readers) and sticky
 * header. Wraps in a horizontally scrollable container so wide data never breaks
 * the page layout on mobile.
 *
 * Columns are declared once; numeric columns get right-aligned mono cells.
 *
 *   <Table caption="Frames tonight" columns={cols} rows={frames} getKey={f => f.id} />
 */
import type { ReactNode } from "react";

export interface Column<Row> {
  key: string;
  header: ReactNode;
  /** Cell renderer. */
  cell: (row: Row) => ReactNode;
  /** Right-align + mono for numeric/size/time columns. */
  numeric?: boolean;
  /** Column min width to keep it readable while scrolling. */
  width?: number | string;
  /** Class for the column's header and cells (e.g. hiding it below a breakpoint). */
  className?: string;
}

export interface TableProps<Row> {
  caption: string;
  /** Hide the caption visually (still announced). Default true. */
  captionHidden?: boolean;
  columns: Column<Row>[];
  rows: Row[];
  getKey: (row: Row, index: number) => string | number;
  /** Rendered in place of the tbody when there are no rows. */
  empty?: ReactNode;
  className?: string;
}

export function Table<Row>({
  caption,
  captionHidden = true,
  columns,
  rows,
  getKey,
  empty,
  className = "",
}: TableProps<Row>) {
  return (
    <div className={`aac-table-wrap ${className}`}>
      <table className="aac-table">
        <caption className={captionHidden ? "sr-only" : undefined}>{caption}</caption>
        <thead>
          <tr>
            {columns.map((c) => (
              <th
                key={c.key}
                scope="col"
                className={c.className}
                style={c.width ? { minWidth: c.width } : undefined}
              >
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && empty ? (
            <tr>
              <td colSpan={columns.length}>{empty}</td>
            </tr>
          ) : (
            rows.map((row, i) => (
              <tr key={getKey(row, i)}>
                {columns.map((c) => (
                  <td
                    key={c.key}
                    className={[c.numeric ? "aac-num" : "", c.className ?? ""].join(" ").trim() || undefined}
                  >
                    {c.cell(row)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}
