import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api, ensureDefinition } from "./api";
import { Floor } from "./Floor";
import { formatDateTime, projectName, taskHeading, taskState } from "./format";
import { useNow, usePolled } from "./polling";
import { navigate } from "./router";
import { loadContinuation } from "./continuation";
import type { Continuation } from "./continuation";
import { automaticGoal, canPublish, defaultModel, starterSource } from "./starters";
import type { Delivery, TaskMode } from "./starters";
import type { Run } from "./types";
import { ErrorBanner, Link, StatusBadge } from "./ui";

function storedProject(): string | null {
  try { return localStorage.getItem("jig-project"); } catch { return null; /* Browser storage can be disabled. */ }
}

export function Tasks() {
  const loadRuns = useCallback(() => api.runs(), []);
  const runs = usePolled(loadRuns, 3000, "tasks");
  const loadFleet = useCallback(() => api.fleet(), []);
  const fleet = usePolled(loadFleet, 3000, "task-workers");
  const [repository, setRepository] = useState(() => new URLSearchParams(window.location.search).get("project") ?? storedProject() ?? "");
  const [task, setTask] = useState("");
  const [mode, setMode] = useState<TaskMode>(() => { const mode = new URLSearchParams(window.location.search).get("mode"); return mode === "ask" || mode === "build" ? mode : "auto"; });
  const [model, setModel] = useState(defaultModel);
  const [testCommand, setTestCommand] = useState("");
  const [delivery, setDelivery] = useState<Delivery>("local");
  const [continueID] = useState(() => new URLSearchParams(window.location.search).get("continue"));
  const [continuation, setContinuation] = useState<Continuation | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [saved, setSaved] = useState(false);
  const [workflowID, setWorkflowID] = useState("");
  const loadDefinitions = useCallback(() => api.definitions(), []);
  const definitions = usePolled(loadDefinitions, 15000, "task-definitions");
  const publishable = !saved && mode !== "ask" && canPublish(repository);
  const publishing = publishable && delivery === "publish";

  useEffect(() => {
    if (!continueID) return;
    let live = true;
    loadContinuation(continueID).then((loaded) => { if (live) { setContinuation(loaded); setRepository(loaded.repository); } },
      (failure: Error) => { if (live) setError(failure); });
    return () => { live = false; };
  }, [continueID]);
  const liveRuntimes = (fleet.data?.workers ?? []).filter((worker) => worker.live).flatMap((worker) => (worker.runtimes ?? []).map((runtime) => runtime.name));
  const codexReady = liveRuntimes.includes("codex");
  const projects = [...new Set((runs.data ?? []).flatMap((run) => (run.targets ?? []).map((target) => target.repository)))];
  // A saved workflow is any definition the task form did not generate. It
  // runs on the runtime its definition pins, or on any live worker.
  const workflows = (definitions.data ?? []).filter((definition) => !definition.name.startsWith("jig-task-"));
  const workflow = saved ? workflows.find((definition) => definition.id === workflowID) ?? workflows[0] : undefined;
  const pinned = workflow ? /^runtime:[ \t]*["']?([\w-]+)/m.exec(workflow.source)?.[1] : undefined;
  const ready = saved ? Boolean(workflow) && (pinned ? liveRuntimes.includes(pinned) : liveRuntimes.length > 0) : codexReady;
  const taskNeeded = saved || mode !== "auto";
  const nowMs = useNow(1000);

  const start = async (event: FormEvent) => {
    event.preventDefault();
    if (pending || !repository.trim() || (taskNeeded && !task.trim()) || (!saved && !model.trim()) || !ready || fleet.error || (continueID && !continuation)) return;
    setPending(true);
    setError(null);
    try {
      const chosen: Delivery = publishing ? "publish" : "local";
      const result = saved && workflow
        ? await api.startRun(workflow.id, repository.trim(), task.trim())
        : await api.startTask((await ensureDefinition(await starterSource(mode, model, testCommand, chosen))).id, repository.trim(), task.trim() || automaticGoal, mode, {
          delivery: chosen, context: continuation?.context, ref: continuation?.ref, continues: continuation?.runID,
        });
      try { localStorage.setItem("jig-project", repository.trim()); } catch { /* Task creation succeeded even if browser storage is disabled. */ }
      navigate(`/runs/${result.run.id}`);
    } catch (failure) {
      setError(failure as Error);
    } finally {
      setPending(false);
    }
  };

  const pick = (next: TaskMode) => { setSaved(false); setMode(next); };

  return <div className="view floor-view">
    <section className="floor-main" aria-label="On the line">
      <header className="floor-head"><h1>On the line</h1><Link href="/runs">All jobs →</Link></header>
      <ErrorBanner error={runs.error} />
      {(runs.data ?? []).length > 0 && <Floor runs={(runs.data ?? []).slice(0, 12)} nowMs={nowMs} />}
      {!runs.loading && (runs.data ?? []).length === 0 && <p className="subtle">The line is empty. Your first job appears here when you press Start.</p>}
      <p className="floor-workers">
        {fleet.data ? (fleet.data.workers ?? []).filter((worker) => worker.live).length === 0 ? "No worker is online. Jobs wait in the queue until one starts." :
          (fleet.data.workers ?? []).filter((worker) => worker.live).map((worker) => `${worker.name} · ${(worker.runtimes ?? []).map((runtime) => runtime.name).join(", ")} · ${worker.active_count}/${worker.capacity} busy`).join("   ") : "Checking workers…"}
      </p>
    </section>
    <form className="task-form floor-form" onSubmit={(event) => void start(event)}>
      <h2>Start a job</h2>
      <fieldset disabled={pending}>
        <label htmlFor="project">1. Project</label>
        <input id="project" list="projects" placeholder="Paste a local folder path or GitHub repository" required value={repository} onChange={(event) => setRepository(event.target.value)} />
        <datalist id="projects">{projects.map((project) => <option value={project} key={project}>{projectName(project)}</option>)}</datalist>
        <p className="subtle">Use a Git project. Work starts from its latest commit; uncommitted edits are not included.</p>
        <fieldset className="mode-choice"><legend>2. What result do you want?</legend>
          <label className={!saved && mode === "auto" ? "selected" : ""}><input type="radio" name="mode" value="auto" checked={!saved && mode === "auto"} onChange={() => pick("auto")} /><span><strong>Let agents improve it</strong><small>Agents choose the directive, plan, build, and test. No task-writing required.</small></span></label>
          <label className={!saved && mode === "ask" ? "selected" : ""}><input type="radio" name="mode" value="ask" checked={!saved && mode === "ask"} onChange={() => pick("ask")} /><span><strong>Get an answer</strong><small>Research and recommendations. No files changed.</small></span></label>
          <label className={!saved && mode === "build" ? "selected" : ""}><input type="radio" name="mode" value="build" checked={!saved && mode === "build"} onChange={() => pick("build")} /><span><strong>Make changes</strong><small>Edit files, run tests, and prepare work for review.</small></span></label>
          <label className={saved ? "selected" : ""}><input type="radio" name="mode" value="saved" checked={saved} disabled={workflows.length === 0} onChange={() => setSaved(true)} /><span><strong>Run a saved workflow</strong><small>{workflows.length === 0 ? "No workflow is saved yet. Save one with jig def create." : "Its own phases, reviews, risk gate and delivery."}</small></span></label>
        </fieldset>
        {saved && <>
          <label htmlFor="workflow">Workflow</label>
          <select id="workflow" value={workflow?.id ?? ""} onChange={(event) => setWorkflowID(event.target.value)}>
            {workflows.map((definition) => <option key={definition.id} value={definition.id}>{definition.name}</option>)}
          </select>
        </>}
        <p className="subtle">{saved ? `Runs on ${pinned ?? "any online runtime"}. Delivery follows the workflow's publish policy.` : mode === "ask" ? "Read-only. Codex reads the project and gives you an answer." : "Plan → build → test. Failed tests restart the loop automatically, up to three repair rounds."}</p>
        {!saved && mode !== "ask" && <fieldset className="mode-choice delivery-choice"><legend>When the checks pass</legend>
          <label className={!publishing ? "selected" : ""}><input type="radio" name="delivery" value="local" checked={!publishing} onChange={() => setDelivery("local")} /><span><strong>Keep it local</strong><small>Changes stay in a saved working folder on this Mac. Nothing is pushed to GitHub.</small></span></label>
          <label className={publishing ? "selected" : ""}><input type="radio" name="delivery" value="publish" disabled={!publishable} checked={publishing} onChange={() => setDelivery("publish")} /><span><strong>Open a pull request</strong><small>{publishable ? "Jig commits the changes, pushes a branch, and opens a pull request with your GitHub login." : "Needs a GitHub project, such as github.com/owner/repo. A local folder can only keep changes local."}</small></span></label>
        </fieldset>}
        {continueID && <div className="notice continuation" role="status">
          {continuation ? <><strong>Continuing:</strong> {continuation.title.split("\n")[0]}
            <p className="subtle">{continuation.ref ? `Starts from the earlier task's branch ${continuation.ref}, so its changes are included.` : continuation.keptLocal ? "The earlier changes were kept local and never pushed, so this task starts from the project's latest commit without them. The earlier directive and result are passed along as context." : "This task starts from the project's latest commit. The earlier directive and result are passed along as context."}</p></> : "Loading the earlier task…"}
        </div>}
        <details className="goal-options" open={taskNeeded}><summary>{taskNeeded ? "Your directive" : "Add a goal or limits (optional)"}</summary>
        <label htmlFor="task">{saved ? "3. Instructions" : mode === "auto" ? "3. Goal or limits (optional)" : "3. What should Codex do?"}</label>
        <textarea id="task" required={taskNeeded} rows={5} maxLength={20000} placeholder={saved ? "One change, stated so a reviewer can check it." : mode === "auto" ? "Leave blank: agents inspect the project and choose one useful improvement. Or give a goal, such as make installation easier." : mode === "ask" ? "Explain what this project does and how I can use it." : "Describe the change you want and what a working result looks like."} value={task} onChange={(event) => setTask(event.target.value)} />
        </details>
        {!saved && <details className="evidence"><summary>Options</summary>
          <label htmlFor="model">Codex model</label>
          <input id="model" required value={model} onChange={(event) => setModel(event.target.value)} />
          {mode !== "ask" && <><label htmlFor="test-command">Test command</label><input id="test-command" placeholder="Leave blank to detect the project's test command" value={testCommand} onChange={(event) => setTestCommand(event.target.value)} /></>}
        </details>}
        <ErrorBanner error={error} />
        <ErrorBanner error={fleet.error} />
        <ErrorBanner error={definitions.error} />
        <div className="start-row">
          <button className="primary" type="submit" disabled={pending || !ready || Boolean(fleet.error) || !repository.trim() || (taskNeeded && !task.trim()) || (!saved && !model.trim()) || Boolean(continueID && !continuation)}>{pending ? "Starting…" : saved ? "Start job" : "Start task"}</button>
          <span className="subtle">{saved
            ? ready ? `Runs ${workflow?.name ?? "the workflow"} on ${pinned ?? "an online worker"}.` : fleet.loading ? "Checking workers…" : `No online worker runs ${pinned ?? "any runtime"}. Check Workers before starting.`
            : codexReady ? `Uses your signed-in Codex. ${publishing ? "Passing work becomes a pull request." : "Results stay local."}` : fleet.loading ? "Checking Codex…" : "Codex is offline. Check Workers before starting."}</span>
        </div>
        {!saved && <p className="task-intent"><strong>{mode === "ask" ? "This task will return an answer." : "This task will change files in a separate working folder."}</strong> {mode === "ask" ? "Choose Make changes if you want Codex to implement something." : publishing ? "Your original project stays untouched; review and merge the pull request on GitHub." : "Your original project stays untouched. Nothing reaches GitHub unless you choose Open a pull request."}</p>}
      </fieldset>
    </form>
  </div>;
}

export function TaskCard({ run }: { run: Run }) {
  const title = taskHeading(run);
  return <article className={`panel task-card task-card-${taskState(run)}`}>
      <div className="task-card-meta"><span className="task-project">{run.targets.map((target) => projectName(target.repository)).join(", ")}</span><StatusBadge state={taskState(run)} /></div>
      <h3><Link href={`/runs/${run.id}`}>{title}</Link></h3>
      <p className="subtle">{formatDateTime(run.created_at)}</p>
      <Link href={`/runs/${run.id}`}>Open task →</Link>
    </article>;
}
