// PlantBar.tsx — the numbers an operator checks before anything else, on
// every view: what was accepted today, whether CI agreed first time, what an
// accept costs, what is waiting on a person. They are `GET /api/report` over
// the local day, read verbatim; a rate with nothing to divide by is "n/a",
// never 0%, exactly as `jig report` prints it.
import { useCallback, useEffect } from "react";
import { api } from "./api";
import { formatUSD } from "./format";
import { KeyboardLayer, openDialog } from "./keyboard";
import { usePolled, useSharedPoll } from "./polling";
import type { Route } from "./router";
import { Link } from "./ui";

function startOfLocalDay(): string {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate()).toISOString();
}

export function lineStatus(liveWorkers: number | undefined, active: number | undefined): string {
  if (liveWorkers === undefined || active === undefined) return "…";
  if (liveWorkers === 0) return "no worker";
  return active > 0 ? "running" : "idle";
}

export function PlantBar({ route }: { route: Route }) {
  const loadReport = useCallback(() => api.report(startOfLocalDay()), []);
  const report = usePolled(loadReport, 15000, "plant:report");
  // Shared with the Floor's worker line and the Queue view: one request
  // serves every reader on screen.
  const fleet = useSharedPoll("workers", api.fleet, 5000);
  const queue = useSharedPoll("queue", api.queue, 5000);
  // A run or job page names itself more exactly once it has loaded; until
  // then the tab says what kind of page it is. The list views set their own.
  useEffect(() => {
    if (route.name === "run" || route.name === "job") document.title = "Job · jig";
    if (route.name === "unknown") document.title = "Not found · jig";
  }, [route.name]);
  const data = report.data;
  const waited = data?.ci.waited ?? 0;
  const status = lineStatus(fleet.data?.live_count, queue.data?.active_count);

  return (
    <header className="plant">
      <Link href="/"><span className="plant-brand">jig<small>line · {status}</small></span></Link>
      <dl className="plant-figures" aria-label="Today">
        <div><dt><Label long="Accepted today" short="Accepted" /></dt><dd>{data ? data.jobs.accepted : "—"}<em>{data ? ` / ${data.jobs.total}` : ""}</em></dd></div>
        <div><dt><Label long="First-pass CI" short="CI 1st" /></dt><dd>{data && waited > 0 ? Math.round((data.ci.first_pass_green / waited) * 100) : "n/a"}<em>{data && waited > 0 ? " %" : ""}</em></dd></div>
        <div><dt><Label long="Cost per accept" short="Per accept" /></dt><dd className="plant-amber">{data ? formatUSD(data.spend.per_clean_accept_usd).replace("—", "n/a") : "—"}</dd></div>
        <div><dt><Label long="Held for a person" short="Held" /></dt><dd className={data && data.publish.held > 0 ? "plant-red" : undefined}>{data ? data.publish.held : "—"}</dd></div>
        <div><dt><Label long="Spend today" short="Spend" /></dt><dd>{data ? formatUSD(data.spend.total_usd) : "—"}</dd></div>
      </dl>
      <nav className="plant-nav" aria-label="Main navigation">
        <Link href="/" current={route.name === "tasks"}>Floor</Link>
        <Link href="/runs" current={["runs", "run", "job"].includes(route.name)}>Jobs</Link>
        <Link href="/queue" current={route.name === "queue"}>Queue</Link>
        <Link href="/fleet" current={route.name === "fleet"}>Workers</Link>
        <Link href="/worktrees" current={route.name === "worktrees"}>Worktrees</Link>
        <button type="button" className="plant-keys" onClick={() => openDialog("help")} aria-label="Keyboard shortcuts" title="Keyboard shortcuts (?)">?</button>
      </nav>
      <KeyboardLayer route={route} />
    </header>
  );
}

/**
 * Label is a figure's name in full, and in a short form a phone shows
 * instead (≤700px). Assistive technology always reads the full name.
 */
function Label({ long, short }: { long: string; short: string }) {
  return <><span className="plant-long">{long}</span><span className="plant-short" aria-hidden="true">{short}</span></>;
}
