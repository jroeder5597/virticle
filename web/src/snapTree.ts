// Turns the flat snapshot list (each with its parent's name) into the rows of a tree view.
export interface TreeSnap {
  name: string;
  parent?: string;
  isCurrent?: boolean;
  created: number;
}

export interface TreeRow<T extends TreeSnap> {
  /** "snap" is a snapshot; "now" is the running state, shown below the current snapshot. */
  kind: "snap" | "now";
  snap?: T;
  /** Snapshot name; the "now" row carries the name of the snapshot it hangs from. */
  name: string;
  depth: number;
  /** The last of its siblings (its elbow closes the branch). */
  isLast: boolean;
  /** For each gutter column left of the elbow (depth 1 .. depth-1): the ancestor there has later siblings, so a trunk line continues. */
  trunk: boolean[];
  hasChildren: boolean;
  /** Row index of the parent, -1 for a root. */
  parent: number;
  /** Number of snapshots below this one (not counting the "now" row). */
  descendants: number;
}

/**
 * Depth-first rows, siblings oldest first. A snapshot whose parent is not in the list is a root. With `withNow`
 * and a current snapshot, a "now" row is added as the last child of the current one.
 */
export function buildSnapRows<T extends TreeSnap>(list: T[], withNow = false): TreeRow<T>[] {
  const have = new Set(list.map((s) => s.name));
  const kids = new Map<string, T[]>();
  const roots: T[] = [];
  for (const s of list) {
    if (s.parent && have.has(s.parent) && s.parent !== s.name) {
      const a = kids.get(s.parent) ?? [];
      a.push(s);
      kids.set(s.parent, a);
    } else roots.push(s);
  }
  const byAge = (a: T, b: T) => a.created - b.created || a.name.localeCompare(b.name);
  roots.sort(byAge);
  for (const a of kids.values()) a.sort(byAge);
  const rows: TreeRow<T>[] = [];
  const seen = new Set<string>();
  const walk = (s: T, depth: number, isLast: boolean, trunk: boolean[], parent: number) => {
    if (seen.has(s.name)) return; // a cycle in bad data must not loop
    seen.add(s.name);
    const children = kids.get(s.name) ?? [];
    const index = rows.length;
    const row: TreeRow<T> = { kind: "snap", snap: s, name: s.name, depth, isLast, trunk, hasChildren: children.length > 0 || (withNow && !!s.isCurrent), parent, descendants: 0 };
    rows.push(row);
    const childTrunk = depth === 0 ? [] : [...trunk, !isLast];
    const showNow = withNow && !!s.isCurrent;
    children.forEach((c, i) => walk(c, depth + 1, i === children.length - 1 && !showNow, childTrunk, index));
    if (showNow) rows.push({ kind: "now", name: s.name, depth: depth + 1, isLast: true, trunk: childTrunk, hasChildren: false, parent: index, descendants: 0 });
    row.descendants = rows.slice(index + 1).filter((r) => r.kind === "snap").length;
  };
  roots.forEach((r, i) => walk(r, 0, i === roots.length - 1, [], -1));
  return rows;
}

/** Names of a snapshot and everything below it. */
export function branchOf<T extends TreeSnap>(rows: TreeRow<T>[], name: string): string[] {
  const i = rows.findIndex((r) => r.kind === "snap" && r.name === name);
  if (i < 0) return [];
  const out = [name];
  for (let j = i + 1; j < rows.length && rows[j].depth > rows[i].depth; j++) if (rows[j].kind === "snap") out.push(rows[j].name);
  return out;
}
