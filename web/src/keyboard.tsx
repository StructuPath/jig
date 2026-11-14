// keyboard.tsx — the keyboard layer: one keydown handler for the whole app,
// the shortcuts dialog (`?`) and the quick-jump palette (Cmd/Ctrl-K). The
// shortcut table is a pure function of the key and what was pressed before
// it, so the bindings are testable without a DOM.
//
// Single keys are ignored while the operator types in a field: `/` in the
// filter box is a slash, not a shortcut. Cmd/Ctrl-K carries a modifier, so it
// works from anywhere.
import { useEffect, useMemo, useRef, useState } from "react";
import type { KeyboardEvent as ReactKeyboardEvent, ReactNode } from "react";
import { api } from "./api";
import { projectName, shortID, taskHeading } from "./format";
import { toggleLayout } from "./listQuery";
import { navigate } from "./router";
import type { Route } from "./router";
import { runJobStatus } from "./summary";
import type { RunListItem } from "./types";

/** useDocumentTitle names the view in the tab: "Jobs · jig". */
export function useDocumentTitle(view: string): void {
  useEffect(() => { document.title = `${view} · jig`; }, [view]);
}

export type ShortcutAction =
  | { kind: "palette" }
  | { kind: "help" }
  | { kind: "focus-filter" }
  | { kind: "toggle-layout" }
  | { kind: "go"; path: string }
  /** The first key of a two-key sequence (`g` then f, j or w). */
  | { kind: "prefix"; key: string };

export interface KeyInput {
  key: string;
  metaKey?: boolean;
  ctrlKey?: boolean;
  altKey?: boolean;
  target?: EventTarget | null;
}

/** GO_TARGETS are the `g` sequences: Floor, Jobs, Workers. */
export const GO_TARGETS: Record<string, { path: string; label: string }> = {
  f: { path: "/", label: "Floor" },
  j: { path: "/runs", label: "Jobs" },
  w: { path: "/fleet", label: "Workers" },
};

/** isTyping says whether a key lands in a field the operator is typing in. */
export function isTyping(target: EventTarget | null | undefined): boolean {
  if (!target || typeof (target as Element).closest !== "function") return false;
  const element = target as HTMLElement;
  if (element.isContentEditable) return true;
  return element.closest("input, textarea, select, [contenteditable='true']") !== null;
}

/**
 * shortcutFor maps one keydown to an action, or null when the key is not a
 * shortcut. `pending` is the prefix key pressed just before, if any.
 */
export function shortcutFor(event: KeyInput, pending: string | null): ShortcutAction | null {
  if ((event.metaKey || event.ctrlKey) && !event.altKey && event.key.toLowerCase() === "k") return { kind: "palette" };
  if (event.metaKey || event.ctrlKey || event.altKey) return null;
  if (isTyping(event.target)) return null;
  if (pending === "g") {
    const target = GO_TARGETS[event.key];
    return target ? { kind: "go", path: target.path } : null;
  }
  switch (event.key) {
    case "/": return { kind: "focus-filter" };
    case "?": return { kind: "help" };
    case "b": return { kind: "toggle-layout" };
    case "g": return { kind: "prefix", key: "g" };
    default: return null;
  }
}

export const SHORTCUTS: ReadonlyArray<{ keys: string[]; does: string }> = [
  { keys: ["⌘ K", "Ctrl K"], does: "Jump to a job or view" },
  { keys: ["/"], does: "Filter the jobs list" },
  { keys: ["b"], does: "Switch Jobs between list and board" },
  { keys: ["g", "f"], does: "Go to the Floor" },
  { keys: ["g", "j"], does: "Go to Jobs" },
  { keys: ["g", "w"], does: "Go to Workers" },
  { keys: ["?"], does: "Show these shortcuts" },
  { keys: ["Esc"], does: "Close a dialog" },
];

/** focusFilter focuses the jobs filter, opening Jobs first when it is not on screen. */
function focusFilter(): void {
  const field = document.getElementById("job-filter");
  if (field) { field.focus(); return; }
  navigate("/runs");
  let tries = 0;
  const retry = () => {
    const next = document.getElementById("job-filter");
    if (next) next.focus();
    else if (++tries < 40) setTimeout(retry, 50);
  };
  setTimeout(retry, 0);
}

const OPEN_EVENT = "jig:open-dialog";

/** openDialog opens the shortcuts dialog or the palette from a button. */
export function openDialog(which: "help" | "palette"): void {
  window.dispatchEvent(new CustomEvent(OPEN_EVENT, { detail: which }));
}

export function KeyboardLayer({ route }: { route: Route }) {
  const [open, setOpen] = useState<"help" | "palette" | null>(null);
  const pending = useRef<{ key: string; timer: ReturnType<typeof setTimeout> } | null>(null);
  const routeRef = useRef(route);
  routeRef.current = route;
  const openRef = useRef(open);
  openRef.current = open;

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.isComposing) return;
      const prefix = pending.current?.key ?? null;
      if (pending.current) { clearTimeout(pending.current.timer); pending.current = null; }
      const action = shortcutFor(event, prefix);
      if (!action) return;
      // Inside an open dialog only the palette toggle applies; the dialog
      // handles its own keys.
      if (openRef.current && action.kind !== "palette") return;
      // `b` means something only where there is a layout to switch.
      if (action.kind === "toggle-layout" && routeRef.current.name !== "runs") return;
      event.preventDefault();
      switch (action.kind) {
        case "palette": setOpen((current) => current === "palette" ? null : "palette"); break;
        case "help": setOpen("help"); break;
        case "focus-filter": focusFilter(); break;
        case "toggle-layout": toggleLayout(); break;
        case "go": navigate(action.path); break;
        case "prefix": pending.current = { key: action.key, timer: setTimeout(() => { pending.current = null; }, 1500) }; break;
      }
    };
    const onOpen = (event: Event) => {
      const which = (event as CustomEvent<"help" | "palette">).detail;
      if (which === "help" || which === "palette") setOpen(which);
    };
    window.addEventListener("keydown", onKey);
    window.addEventListener(OPEN_EVENT, onOpen);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener(OPEN_EVENT, onOpen);
    };
  }, []);

  const close = () => setOpen(null);
  if (open === "help") return <ShortcutsDialog onClose={close} />;
  if (open === "palette") return <Palette onClose={close} />;
  return null;
}

/**
 * Dialog is a modal with a focus trap: Tab and Shift-Tab cycle inside it,
 * Escape closes it, and focus returns to where it was when it opened.
 */
export function Dialog({ label, onClose, children, className = "" }: { label: string; onClose: () => void; children: ReactNode; className?: string }) {
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const first = box.current?.querySelector<HTMLElement>("[data-autofocus]") ?? box.current;
    first?.focus();
    return () => { previous?.focus?.(); };
  }, []);
  const onKeyDown = (event: ReactKeyboardEvent) => {
    if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); onClose(); return; }
    if (event.key !== "Tab" || !box.current) return;
    const focusable = [...box.current.querySelectorAll<HTMLElement>("a[href], button:not([disabled]), input, select, textarea, [tabindex]:not([tabindex='-1'])")];
    if (focusable.length === 0) { event.preventDefault(); return; }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };
  return <div className="dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <div ref={box} className={`dialog ${className}`} role="dialog" aria-modal="true" aria-label={label} tabIndex={-1} onKeyDown={onKeyDown}>
      {children}
    </div>
  </div>;
}

function ShortcutsDialog({ onClose }: { onClose: () => void }) {
  return <Dialog label="Keyboard shortcuts" onClose={onClose}>
    <header className="dialog-head"><h2>Keyboard shortcuts</h2><button type="button" onClick={onClose} data-autofocus>Close</button></header>
    <table className="shortcuts">
      <tbody>
        {SHORTCUTS.map((shortcut) => <tr key={shortcut.does}>
          <td>{shortcut.keys.map((key, index) => <span key={key}>{index > 0 && <span className="subtle">{shortcut.keys[0] === "g" ? " then " : " or "}</span>}<kbd>{key}</kbd></span>)}</td>
          <td>{shortcut.does}</td>
        </tr>)}
      </tbody>
    </table>
  </Dialog>;
}

export interface PaletteItem {
  id: string;
  label: string;
  detail: string;
  href: string;
  /** IDs a prefix of which selects the item: the run's and its jobs'. */
  ids: string[];
  /** Views rank under jobs only when the query does not name a job. */
  view?: boolean;
}

const VIEWS: PaletteItem[] = [
  { id: "view:/", label: "Floor", detail: "View", href: "/", ids: [], view: true },
  { id: "view:/runs", label: "Jobs", detail: "View", href: "/runs", ids: [], view: true },
  { id: "view:/queue", label: "Queue", detail: "View", href: "/queue", ids: [], view: true },
  { id: "view:/fleet", label: "Workers", detail: "View", href: "/fleet", ids: [], view: true },
  { id: "view:/worktrees", label: "Worktrees", detail: "View", href: "/worktrees", ids: [], view: true },
];

/** paletteItems are the jumps the palette offers: one per job, then the views. */
export function paletteItems(runs: RunListItem[]): PaletteItem[] {
  const jobs = runs.flatMap((run): PaletteItem[] => {
    const summaries = run.job_summaries ?? [];
    const status = runJobStatus(run);
    const title = taskHeading(run);
    if (summaries.length === 0) {
      return [{ id: run.id, label: title, detail: `${(run.targets ?? []).map((target) => projectName(target.repository)).join(", ")} · ${shortID(run.id)}`, href: `/runs/${run.id}`, ids: [run.id] }];
    }
    return summaries.map((summary) => ({
      id: summary.job.id,
      label: title,
      detail: `${projectName(summary.job.repository)} · ${status?.word ?? summary.job.state} · ${shortID(summary.job.id)}`,
      href: `/jobs/${summary.job.id}`,
      ids: [summary.job.id, run.id],
    }));
  });
  return [...jobs, ...VIEWS];
}

/**
 * paletteMatches ranks items for a query: an ID prefix first (a pasted job
 * ID goes straight to its job), then a title or view name that starts with
 * the query, then one that contains it anywhere, then a match in the detail
 * line (the project). An empty query lists the views, then recent jobs.
 */
export function paletteMatches(items: PaletteItem[], query: string, limit = 9): PaletteItem[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return [...items.filter((item) => item.view), ...items.filter((item) => !item.view)].slice(0, limit);
  const scored: { item: PaletteItem; score: number; index: number }[] = [];
  items.forEach((item, index) => {
    const label = item.label.toLowerCase();
    let score = -1;
    if (item.ids.some((id) => id.toLowerCase().startsWith(needle))) score = 0;
    else if (label.startsWith(needle)) score = item.view ? 1 : 2;
    else if (label.includes(needle)) score = 3;
    else if (item.detail.toLowerCase().includes(needle)) score = 4;
    if (score >= 0) scored.push({ item, score, index });
  });
  return scored.sort((left, right) => left.score - right.score || left.index - right.index).slice(0, limit).map((entry) => entry.item);
}

function Palette({ onClose }: { onClose: () => void }) {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const [runs, setRuns] = useState<RunListItem[] | null>(null);
  const [error, setError] = useState<Error | null>(null);
  useEffect(() => {
    let live = true;
    api.runs().then((list) => { if (live) setRuns(list); }, (failure: Error) => { if (live) setError(failure); });
    return () => { live = false; };
  }, []);
  const items = useMemo(() => paletteItems(runs ?? []), [runs]);
  const matches = paletteMatches(items, query);
  const selected = Math.min(active, Math.max(0, matches.length - 1));
  const go = (item: PaletteItem | undefined) => {
    if (!item) return;
    onClose();
    navigate(item.href);
  };
  const onKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown") { event.preventDefault(); setActive((selected + 1) % Math.max(1, matches.length)); }
    else if (event.key === "ArrowUp") { event.preventDefault(); setActive((selected - 1 + matches.length) % Math.max(1, matches.length)); }
    else if (event.key === "Enter") { event.preventDefault(); go(matches[selected]); }
  };

  return <Dialog label="Jump to" onClose={onClose} className="palette">
    <input
      data-autofocus
      className="palette-input"
      role="combobox"
      aria-expanded="true"
      aria-controls="palette-list"
      aria-activedescendant={matches[selected] ? `palette-${matches[selected].id}` : undefined}
      aria-label="Job ID, title or view"
      placeholder="Job ID, title or view…"
      value={query}
      onChange={(event) => { setQuery(event.target.value); setActive(0); }}
      onKeyDown={onKeyDown}
    />
    <ul id="palette-list" role="listbox" aria-label="Matches" className="palette-list">
      {matches.map((item, index) => <li
        key={item.id}
        id={`palette-${item.id}`}
        role="option"
        aria-selected={index === selected}
        className={index === selected ? "selected" : undefined}
        onMouseEnter={() => setActive(index)}
        onMouseDown={(event) => { event.preventDefault(); go(item); }}
      ><span className="palette-label">{item.label}</span><span className="palette-detail">{item.detail}</span></li>)}
      {matches.length === 0 && <li className="palette-none" role="presentation">{runs ? "No job or view matches." : error ? `Jobs could not be read: ${error.message}` : "Loading jobs…"}</li>}
    </ul>
    <p className="palette-hint subtle">↑ ↓ to choose · Enter to open · Esc to close</p>
  </Dialog>;
}
