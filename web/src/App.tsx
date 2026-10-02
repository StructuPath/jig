// App.tsx — six views, one nav. There is deliberately no overview-metrics
// dashboard: the plan defers it, and the events table keeps the data for
// whenever it stops being a distraction from the queue and the traces.
//
// Fleet sits next to Queue on purpose. The two answer one question between
// them — the queue says what is waiting, the fleet says whether anything can
// pick it up — and an operator staring at a queue that will not drain needs
// the second half one click away.
import { Fleet } from "./Fleet";
import { Queue, Runs } from "./Queue";
import { JobDetail, RunDetail } from "./RunDetail";
import { useRoute } from "./router";
import { Empty, Link } from "./ui";
import { Worktrees } from "./Worktrees";
import { Tasks } from "./Tasks";

export function App() {
  const route = useRoute();
  return (
    <div className="app">
      <nav className="nav" aria-label="Main navigation">
        <span className="brand">jig<span className="brand-caption">software factory</span></span>
        <span className="nav-caption">WORKSPACE</span>
        <Link href="/" current={route.name === "tasks"}><span aria-hidden="true">＋</span> New task</Link>
        <Link href="/runs" current={["runs", "run", "job"].includes(route.name)}><span aria-hidden="true">▦</span> My tasks</Link>
        <details className="nav-tools"><summary>Tools</summary>
          <Link href="/queue" current={route.name === "queue"}>Work queue</Link>
          <Link href="/fleet" current={route.name === "fleet"}>Workers</Link>
          <Link href="/worktrees" current={route.name === "worktrees"}>Saved worktrees</Link>
        </details>
        <div className="nav-footer"><span className="nav-caption">LOCAL BY DESIGN</span><p>Your projects.<br />Your Codex.<br />Your next move.</p></div>
      </nav>
      <main>
        {route.name === "tasks" && <Tasks />}
        {route.name === "queue" && <Queue />}
        {route.name === "runs" && <Runs />}
        {route.name === "run" && <RunDetail runID={route.id} />}
        {route.name === "job" && <JobDetail jobID={route.id} />}
        {route.name === "worktrees" && <Worktrees />}
        {route.name === "fleet" && <Fleet />}
        {route.name === "unknown" && (
          <Empty title="No such view" detail={`Nothing is served at ${route.path}.`} />
        )}
      </main>
    </div>
  );
}
