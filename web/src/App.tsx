// App.tsx — six views under one plant bar. The bar carries the day's report
// figures and the navigation, so the answer to "is the line running and what
// does an accept cost" is on every view, not on a dashboard of its own.
//
// Workers sits next to Queue on purpose. The two answer one question between
// them — the queue says what is waiting, the fleet says whether anything can
// pick it up — and an operator staring at a queue that will not drain needs
// the second half one click away.
import { Fleet } from "./Fleet";
import { PlantBar } from "./PlantBar";
import { Queue, Runs } from "./Queue";
import { JobDetail, RunDetail } from "./RunDetail";
import { useRoute } from "./router";
import { Empty } from "./ui";
import { Worktrees } from "./Worktrees";
import { Tasks } from "./Tasks";

export function App() {
  const route = useRoute();
  return (
    <div className="app">
      <PlantBar route={route} />
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
