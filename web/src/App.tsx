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

export function App() {
  const route = useRoute();
  return (
    <div className="app">
      <nav className="nav">
        <span className="brand">jig</span>
        <Link href="/">Queue</Link>
        <Link href="/fleet">Fleet</Link>
        <Link href="/runs">Runs</Link>
        <Link href="/worktrees">Worktrees</Link>
      </nav>
      <main>
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
