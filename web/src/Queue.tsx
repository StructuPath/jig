// Queue.tsx — the work queue with its depth. Depth is the first number an
// operator wants ("is anything waiting?") and the one a run list cannot
// answer, so it is the headline, not a column.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api } from "./api";
import { columns } from "./board";
import { TaskBoard } from "./TaskBoard";
import { Floor } from "./Floor";
import { formatAge, formatClock, formatDateTime, projectName } from "./format";
import { useDocumentTitle } from "./keyboard";
import { chooseLayout, layoutOf, updateListQuery, useListQuery } from "./listQuery";
import { useNow, usePolled, useSharedPoll } from "./polling";
import { workerCommand } from "./startHelp";
import type { ColumnID } from "./status";
import { runColumnOf, runMatches, runRepositories, sortByAttention } from "./summary";
import type { RunListItem, RunListPage } from "./types";
import { ErrorBanner, Link, Loading, StatusBadge } from "./ui";

export function Queue() {
  const { data, error, loading } = useSharedPoll("queue", api.queue, 2000);
  const fleet = useSharedPoll("workers", api.fleet, 3000);
  const nowMs = useNow(30000);
  useDocumentTitle("Queue");

  if (loading && !data) return <Loading label="Loading queue…" />;
  if (!data) return <ErrorBanner error={error} />;
  const entries = data.entries ?? [];

  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Work queue</h1>
          <p className="subtle">Live work appears here. Finished jobs are in Jobs.</p>
          <p className="subtle">Observed {formatClock(data.observed_at)}</p>
        </div>
        <div className="counters">
          <div className="counter">
            <span className="counter-value">{data.depth}</span>
            <span className="counter-label">queued</span>
          </div>
          <div className="counter">
            <span className="counter-value">{data.active_count}</span>
            <span className="counter-label">running</span>
          </div>
        </div>
      </header>
      <ErrorBanner error={error} />
      <section className="panel readiness" aria-label="Worker readiness">
        <h2>{fleet.data ? `${fleet.data.live_count} worker${fleet.data.live_count === 1 ? "" : "s"} online` : "Checking workers…"}</h2>
        <p>{fleet.data?.live_count ?
          `Ready runtimes: ${(fleet.data.workers ?? []).filter((worker) => worker.live).flatMap((worker) => (worker.runtimes ?? []).map((runtime) => runtime.name)).join(", ")}. Jobs start when a compatible worker has a free slot.` :
          <>Jobs need an online worker before they can start. Run <code>{workerCommand("codex")}</code> for the Floor's jobs, or <code>{workerCommand()}</code> for Claude Code workflows.</>}</p>
        <Link href="/fleet">View workers →</Link>
        <ErrorBanner error={fleet.error} />
      </section>

      {entries.length === 0 ? (
        <section className="panel">
          <h2>No work is running</h2>
          <p>The queue is empty. Opening an old run does not restart it.</p>
          <p>Start a job on the <Link href="/">Floor</Link>, or admit a run of a saved workflow from a terminal:</p>
          <pre>jig def invoke &lt;workflow&gt; --repo &lt;path&gt; --instructions "&lt;what to do&gt;"</pre>
          <Link href="/runs">Browse finished jobs →</Link>
        </section>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>#</th>
              <th>Repository</th>
              <th>Definition</th>
              <th>State</th>
              <th>Attempt</th>
              <th>Enqueued</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={entry.job_id}>
                <td>{entry.position ?? "—"}</td>
                <td>
                  <Link href={`/jobs/${entry.job_id}`}>{entry.repository}</Link>
                </td>
                <td>{entry.definition ?? "—"}</td>
                <td>
                  <StatusBadge state={entry.state} />
                  {entry.state === "queued" && entry.requires_merge && <small>Requires a worker with --allow-merge</small>}
                </td>
                <td>
                  {entry.attempt ? (
                    <>
                      #{entry.attempt.attempt_number} <StatusBadge state={entry.attempt.state} />
                    </>
                  ) : (
                    "—"
                  )}
                </td>
                <td className="cell-nowrap" title={formatDateTime(entry.enqueued_at)}>{formatAge(entry.enqueued_at, nowMs)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export function Runs() {
  const query = useListQuery();
  const layout = layoutOf(query);
  useDocumentTitle(layout === "board" ? "Board" : "Jobs");
  const load = useCallback(() => api.runsPage(), []);
  const first = usePolled(load, 4000, "runs");
  const older = useOlderRuns(first.data);
  const nowMs = useNow(30000);
  const all = useMemo(() => mergeRuns(first.data?.runs ?? [], older.runs), [first.data, older.runs]);
  const repos = useMemo(() => [...new Set(all.flatMap(runRepositories))].sort(), [all]);
  const filter = { status: query.status, repo: query.repo, q: query.q };
  const matching = all.filter((run) => runMatches(run, { ...filter, status: null }));
  const shown = sortByAttention(matching.filter((run) => !filter.status || runColumnOf(run) === filter.status));

  if (first.loading && !first.data) return <Loading label="Loading jobs…" />;
  if (!first.data) return <ErrorBanner error={first.error} />;
  const filtered = Boolean(filter.status || filter.repo || filter.q);

  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Jobs</h1>
          <p className="subtle">Needs review first, then stopped, running, waiting and done; newest first within each. Press <kbd>?</kbd> for shortcuts.</p>
        </div>
        <div className="layout-switch" role="group" aria-label="Layout">
          <button type="button" aria-pressed={layout === "list"} onClick={() => chooseLayout("list")}>List</button>
          <button type="button" aria-pressed={layout === "board"} onClick={() => chooseLayout("board")}>Board</button>
        </div>
      </header>
      <ErrorBanner error={first.error} />
      {all.length === 0 ? (
        <section className="panel">
          <h2>No jobs yet</h2>
          <p>Start a job on the <Link href="/">Floor</Link>, or admit a run of a saved workflow from a terminal:</p>
          <pre>jig def invoke &lt;workflow&gt; --repo &lt;path&gt; --instructions "&lt;what to do&gt;"</pre>
        </section>
      ) : <>
        <JobFilters runs={matching} repos={repos} status={filter.status} repo={filter.repo} q={filter.q} />
        {shown.length === 0 ? (
          <p className="subtle list-none">No job matches these filters. <button type="button" className="link-button" onClick={() => updateListQuery({ status: null, repo: "", q: "" })}>Clear filters</button></p>
        ) : layout === "board" ? (
          <TaskBoard runs={shown} />
        ) : (
          <Floor runs={shown} nowMs={nowMs} />
        )}
        <p className="list-foot subtle">
          {filtered ? `${shown.length} of ${all.length} jobs shown. ` : `${all.length} jobs. `}
          {older.next(first.data) ? <button type="button" onClick={older.load} disabled={older.loading}>{older.loading ? "Loading…" : "Load older jobs"}</button> : "These are all the jobs."}
        </p>
        <ErrorBanner error={older.error} />
      </>}
    </div>
  );
}

/** mergeRuns joins the polled first page and the older pages, the first page's rows winning. */
export function mergeRuns(first: RunListItem[], older: RunListItem[]): RunListItem[] {
  const seen = new Set(first.map((run) => run.id));
  return [...first, ...older.filter((run) => !seen.has(run.id))];
}

/**
 * useOlderRuns pages back through the list with `before` and the
 * X-Next-Before cursor. Older pages are read once, not polled: the first
 * page is what moves. Once older pages are held, a run the first page
 * pushes down is kept, so a new job cannot open a gap between the pages.
 */
function useOlderRuns(first: RunListPage | null) {
  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [cursor, setCursor] = useState<string | null | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const previous = useRef<RunListItem[]>([]);
  useEffect(() => {
    if (!first) return;
    const held = previous.current;
    previous.current = first.runs;
    if (cursor === undefined || held.length === 0) return;
    const now = new Set(first.runs.map((run) => run.id));
    const pushed = held.filter((run) => !now.has(run.id));
    if (pushed.length) setRuns((current) => mergeRuns(pushed, current));
  }, [first, cursor]);
  const next = (page: RunListPage | null) => cursor === undefined ? page?.next ?? null : cursor;
  const load = () => {
    const before = next(first);
    if (!before || loading) return;
    setLoading(true);
    api.runsPage(before).then((page) => {
      setRuns((current) => mergeRuns(current, page.runs));
      setCursor(page.next);
      setError(null);
    }, (failure: Error) => setError(failure)).finally(() => setLoading(false));
  };
  return { runs, loading, error, load, next };
}

const STATUS_CHIPS: ReadonlyArray<{ id: ColumnID | null; label: string }> = [
  { id: null, label: "All" },
  ...columns.map((column) => ({ id: column.id, label: column.label })),
];

/**
 * JobFilters are the status chips (with counts under the other filters), the
 * repository picker and the text filter. Each writes the URL query, so the
 * filtered list survives a reload and can be shared.
 */
function JobFilters({ runs, repos, status, repo, q }: { runs: RunListItem[]; repos: string[]; status: ColumnID | null; repo: string; q: string }) {
  const counts = new Map<ColumnID, number>();
  for (const run of runs) counts.set(runColumnOf(run), (counts.get(runColumnOf(run)) ?? 0) + 1);
  const [text, setText] = useState(q);
  // The field follows the URL when it changes elsewhere (Back, a link).
  useEffect(() => { setText(q); }, [q]);
  return <div className="job-filters" role="search" aria-label="Filter jobs">
    <div className="chips" role="group" aria-label="Status">
      {STATUS_CHIPS.map((chip) => {
        const count = chip.id ? counts.get(chip.id) ?? 0 : runs.length;
        return <button key={chip.label} type="button" className={`chip${chip.id ? ` chip-${chip.id}` : ""}`} aria-pressed={status === chip.id}
          onClick={() => updateListQuery({ status: chip.id })}>{chip.label} <span className="chip-count">{count}</span></button>;
      })}
    </div>
    <label className="visually-hidden" htmlFor="job-repo">Repository</label>
    <select id="job-repo" value={repo} onChange={(event) => updateListQuery({ repo: event.target.value })}>
      <option value="">All repositories</option>
      {repo && !repos.includes(repo) && <option value={repo}>{projectName(repo)}</option>}
      {repos.map((name) => <option key={name} value={name} title={name}>{projectName(name)}</option>)}
    </select>
    <label className="visually-hidden" htmlFor="job-filter">Filter by title, workflow, repository or ID</label>
    <input id="job-filter" type="search" placeholder="Filter jobs  /" value={text} autoComplete="off"
      onChange={(event) => { setText(event.target.value); updateListQuery({ q: event.target.value }); }}
      onKeyDown={(event) => { if (event.key === "Escape" && text) { event.preventDefault(); setText(""); updateListQuery({ q: "" }); } else if (event.key === "Escape") (event.target as HTMLInputElement).blur(); }} />
  </div>;
}
