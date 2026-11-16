// The footer under a paged list (the fleet, an app team's fleet, the audit trail): where this page
// sits in the whole, read from the server's total and the page it served, never estimated, and a
// way to any page and to the rows a page holds. Every choice is a request: go writes the address.
// On a phone the numbers but the current one fold away; First and Last stay.
export const PAGE_SIZES = [25, 50, 100, 200];
export const PAGE_SIZE = 50;

const count = (n: number) => n.toLocaleString("en-US");

// The pages a footer names: the first, the last and the ones beside this one, with null where pages
// are left out ("1 … 4 5 6 … 26"); a gap of one page shows that page, never an ellipsis in its place.
export function pageList(page: number, pages: number): (number | null)[] {
  const shown = [...new Set([1, page - 1, page, page + 1, pages])].filter((n) => n >= 1 && n <= pages).sort((a, b) => a - b);
  const out: (number | null)[] = [];
  let last = 0;
  for (const n of shown) {
    if (n - last === 2) out.push(last + 1);
    else if (n - last > 2) out.push(null);
    out.push(n);
    last = n;
  }
  return out;
}

const STEP = "h-8 min-w-8 rounded-[7px] border border-border-strong bg-surface px-2.5 text-md font-medium text-text hover:bg-surface-2 disabled:cursor-not-allowed disabled:border-border disabled:bg-surface-2 disabled:text-text-faint";
const CURRENT = "h-8 min-w-8 rounded-[7px] border border-accent bg-accent px-2.5 text-md font-semibold text-accent-ink";

export function Pager({ label, noun, total, page, limit, go }: {
  label: string;
  noun: [one: string, many: string];
  total: number;
  page: number;
  limit: number;
  // limit is sent with the page that keeps this page's first row in view
  go: (to: { page: number; limit: number }) => void;
}) {
  if (total === 0) return null;
  const pages = Math.ceil(total / limit);
  const first = (page - 1) * limit + 1;
  const sizes = PAGE_SIZES.includes(limit) ? PAGE_SIZES : [...PAGE_SIZES, limit].sort((a, b) => a - b);
  const to = (n: number) => () => go({ page: n, limit });
  return (
    <nav aria-label={label} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2.5 text-md text-text-dim">
      <p className="tnum">
        {count(first)}–{count(Math.min(page * limit, total))} of {count(total)} {noun[total === 1 ? 0 : 1]}
        {pages > 1 && <> · Page {count(page)} of {count(pages)}</>}
      </p>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2.5">
        {(pages > 1 || total > PAGE_SIZES[0]!) && (
          <label className="flex items-center gap-2">
            Rows per page
            <select className="h-8 rounded-[7px] border border-border-strong bg-surface px-2 text-md text-text" value={limit}
              onChange={(event) => { const size = Number(event.target.value); go({ page: Math.floor((first - 1) / size) + 1, limit: size }); }}>
              {sizes.map((size) => <option key={size} value={size}>{size}</option>)}
            </select>
          </label>
        )}
        {pages > 1 && (
          <ul className="flex flex-wrap items-center gap-1">
            <li><button type="button" className={STEP} disabled={page === 1} onClick={to(1)}>First</button></li>
            <li><button type="button" className={STEP} disabled={page === 1} onClick={to(page - 1)}><span aria-hidden="true">← </span>Previous</button></li>
            {pageList(page, pages).map((n, i) => n === null
              ? <li key={`gap-${i}`} aria-hidden="true" className="px-1 text-text-faint max-[520px]:hidden">…</li>
              : (
                <li key={n} className={n === page ? "" : "max-[520px]:hidden"}>
                  <button type="button" aria-label={`Page ${count(n)}`} aria-current={n === page ? "page" : undefined} className={`tnum ${n === page ? CURRENT : STEP}`} onClick={to(n)}>{count(n)}</button>
                </li>
              ))}
            <li><button type="button" className={STEP} disabled={page === pages} onClick={to(page + 1)}>Next<span aria-hidden="true"> →</span></button></li>
            <li><button type="button" className={STEP} disabled={page === pages} onClick={to(pages)}>Last</button></li>
          </ul>
        )}
      </div>
    </nav>
  );
}
