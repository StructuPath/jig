// App.tsx — five views, one nav. There is deliberately no overview-metrics
// dashboard: the plan defers it, and the events table keeps the data for
// whenever it stops being a distraction from the queue and the traces.
import { Queue, Runs } from "./Queue";
import { JobDetail, RunDetail } from "./RunDetail";
import { useRoute } from "./router";
import { Empty, Link } from "./ui";
import { Worktrees } from "./Worktrees";

export function App() {
  const route = useRoute();
  return (
    <div className="app">
      <nav className="nav">
        <span className="brand">jig</span>
        <Link href="/">Queue</Link>
        <Link href="/runs">Runs</Link>
        <Link href="/worktrees">Worktrees</Link>
      </nav>
      <main>
        {route.name === "queue" && <Queue />}
        {route.name === "runs" && <Runs />}
        {route.name === "run" && <RunDetail runID={route.id} />}
        {route.name === "job" && <JobDetail jobID={route.id} />}
        {route.name === "worktrees" && <Worktrees />}
        {route.name === "unknown" && (
          <Empty title="No such view" detail={`Nothing is served at ${route.path}.`} />
        )}
      </main>
    </div>
  );
}
