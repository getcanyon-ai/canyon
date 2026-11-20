// lineDiff is the lines of b against a's, each kept (" "), removed ("-") or added ("+"), in order: a longest common
// subsequence of the two bodies' lines, as a diff between two versions of a playbook reads.
// ponytail: O(n·m) table, fine for a 64 KiB body; Myers' diff if a body ever outgrows it.
export interface DiffLine { op: " " | "-" | "+"; line: string }

export function lineDiff(a: string, b: string): DiffLine[] {
  const x = a === "" ? [] : a.replace(/\n$/, "").split("\n");
  const y = b === "" ? [] : b.replace(/\n$/, "").split("\n");
  const w = y.length + 1;
  // at(i, j) is the length of the longest common subsequence of x[i:] and y[j:]
  const table = new Uint32Array((x.length + 1) * w);
  const at = (i: number, j: number) => table[i * w + j] ?? 0;
  for (let i = x.length - 1; i >= 0; i--) {
    for (let j = y.length - 1; j >= 0; j--) table[i * w + j] = x[i] === y[j] ? at(i + 1, j + 1) + 1 : Math.max(at(i + 1, j), at(i, j + 1));
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < x.length || j < y.length) {
    const left = x[i];
    const right = y[j];
    if (left !== undefined && left === right) { out.push({ op: " ", line: left }); i++; j++; }
    else if (left !== undefined && (right === undefined || at(i + 1, j) >= at(i, j + 1))) { out.push({ op: "-", line: left }); i++; }
    else if (right !== undefined) { out.push({ op: "+", line: right }); j++; }
  }
  return out;
}
