// listQuery.ts — the Jobs list's filters and layout, kept in the URL query
// (`/runs?status=stopped&repo=…&q=…&view=board`) so a filtered list survives
// a reload and can be shared as a link. The layout falls back to the stored
// preference when the URL does not name one, so a bare /runs still opens the
// way the operator left it (2026-10-02-001 KTD4).
//
// The query is rewritten with replaceState, not pushState: typing in the
// filter must not leave one history entry per keystroke.
import { useEffect, useState } from "react";
import { columns } from "./board";
import type { ColumnID } from "./status";

export type Layout = "list" | "board";

export interface ListQuery {
  status: ColumnID | null;
  repo: string;
  q: string;
  /** The layout the URL names, or null to use the stored preference. */
  view: Layout | null;
}

const LAYOUT_KEY = "jig-tasks-layout";
const QUERY_EVENT = "jig:list-query";

export function parseListQuery(search: string): ListQuery {
  const params = new URLSearchParams(search);
  const status = params.get("status");
  const view = params.get("view");
  return {
    status: columns.some((column) => column.id === status) ? status as ColumnID : null,
    repo: params.get("repo") ?? "",
    q: params.get("q") ?? "",
    view: view === "board" || view === "list" ? view : null,
  };
}

/** listQuerySearch writes a query back, leaving out what is unset. */
export function listQuerySearch(query: ListQuery): string {
  const params = new URLSearchParams();
  if (query.status) params.set("status", query.status);
  if (query.repo) params.set("repo", query.repo);
  if (query.q) params.set("q", query.q);
  if (query.view) params.set("view", query.view);
  const text = params.toString();
  return text ? `?${text}` : "";
}

export function storedLayout(): Layout {
  try { return localStorage.getItem(LAYOUT_KEY) === "board" ? "board" : "list"; } catch { return "list"; /* Browser storage can be disabled. */ }
}

/** layoutOf is the layout to show: the URL's, else the stored preference. */
export function layoutOf(query: ListQuery): Layout {
  return query.view ?? storedLayout();
}

/** updateListQuery merges a change into the current URL's query. */
export function updateListQuery(patch: Partial<ListQuery>): void {
  const next = { ...parseListQuery(window.location.search), ...patch };
  window.history.replaceState(window.history.state, "", `${window.location.pathname}${listQuerySearch(next)}${window.location.hash}`);
  window.dispatchEvent(new Event(QUERY_EVENT));
}

/** chooseLayout shows a layout, names it in the URL, and stores it as the preference. */
export function chooseLayout(layout: Layout): void {
  try { localStorage.setItem(LAYOUT_KEY, layout); } catch { /* The switch still applies for this visit. */ }
  updateListQuery({ view: layout });
}

/** toggleLayout is the `b` shortcut: list to board and back. */
export function toggleLayout(): void {
  chooseLayout(layoutOf(parseListQuery(window.location.search)) === "board" ? "list" : "board");
}

/** useListQuery reads the query and follows every change to it. */
export function useListQuery(): ListQuery {
  const [search, setSearch] = useState(() => window.location.search);
  useEffect(() => {
    const update = () => setSearch(window.location.search);
    window.addEventListener("popstate", update);
    window.addEventListener(QUERY_EVENT, update);
    return () => {
      window.removeEventListener("popstate", update);
      window.removeEventListener(QUERY_EVENT, update);
    };
  }, []);
  return parseListQuery(search);
}
