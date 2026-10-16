import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api, ensureDefinition } from "./api";
import { formatDateTime, projectName, taskState, taskTitle } from "./format";
import { usePolled } from "./polling";
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
  const publishable = mode !== "ask" && canPublish(repository);
  const publishing = publishable && delivery === "publish";

  useEffect(() => {
    if (!continueID) return;
    let live = true;
    loadContinuation(continueID).then((loaded) => { if (live) { setContinuation(loaded); setRepository(loaded.repository); } },
      (failure: Error) => { if (live) setError(failure); });
    return () => { live = false; };
  }, [continueID]);
  const codexReady = (fleet.data?.workers ?? []).some((worker) => worker.live && worker.runtimes?.some((runtime) => runtime.name === "codex"));
  const projects = [...new Set((runs.data ?? []).flatMap((run) => run.targets.map((target) => target.repository)))];

  const start = async (event: FormEvent) => {
    event.preventDefault();
    if (pending || !repository.trim() || (mode !== "auto" && !task.trim()) || !model.trim() || !codexReady || fleet.error || (continueID && !continuation)) return;
    setPending(true);
    setError(null);
    try {
      const chosen: Delivery = publishing ? "publish" : "local";
      const definition = await ensureDefinition(await starterSource(mode, model, testCommand, chosen));
      const result = await api.startTask(definition.id, repository.trim(), task.trim() || automaticGoal, mode, {
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

  return <div className="view workspace">
    <header className="view-header workspace-hero">
      <div><span className="eyebrow">IDEA IN. WORKING SOFTWARE OUT.</span><h1>Make your <span>next move.</span></h1><p className="subtle">Give agents a project. They find the work, build it, and prove it.</p></div>
      <div className="runtime-status"><StatusBadge state={codexReady && !fleet.error ? "live" : "stale"} /><span>Codex connection</span></div>
    </header>
    <div className="workspace-grid">
    <form className="panel task-form" onSubmit={(event) => void start(event)}>
      <div className="composer-heading"><span className="eyebrow">THE WORKBENCH</span><h2>Let’s build something.</h2><span className="composer-mark" aria-hidden="true">↗</span></div>
      <fieldset disabled={pending}>
        <label htmlFor="project">1. Project</label>
        <input id="project" list="projects" placeholder="Paste a local folder path or GitHub repository" required value={repository} onChange={(event) => setRepository(event.target.value)} />
        <datalist id="projects">{projects.map((project) => <option value={project} key={project}>{projectName(project)}</option>)}</datalist>
        <p className="subtle">Use a Git project. Work starts from its latest commit; uncommitted edits are not included.</p>
        <fieldset className="mode-choice"><legend>2. What result do you want?</legend>
          <label className={mode === "auto" ? "selected" : ""}><input type="radio" name="mode" value="auto" checked={mode === "auto"} onChange={() => setMode("auto")} /><span><strong>Let agents improve it</strong><small>Agents choose the directive, plan, build, and test. No task-writing required.</small></span></label>
          <label className={mode === "ask" ? "selected" : ""}><input type="radio" name="mode" value="ask" checked={mode === "ask"} onChange={() => setMode("ask")} /><span><strong>Get an answer</strong><small>Research and recommendations. No files changed.</small></span></label>
          <label className={mode === "build" ? "selected" : ""}><input type="radio" name="mode" value="build" checked={mode === "build"} onChange={() => setMode("build")} /><span><strong>Make changes</strong><small>Edit files, run tests, and prepare work for review.</small></span></label>
        </fieldset>
        <p className="subtle">{mode === "ask" ? "Read-only. Codex reads the project and gives you an answer." : "Plan → build → test. Failed tests restart the loop automatically, up to three repair rounds."}</p>
        {mode !== "ask" && <fieldset className="mode-choice delivery-choice"><legend>When the checks pass</legend>
          <label className={!publishing ? "selected" : ""}><input type="radio" name="delivery" value="local" checked={!publishing} onChange={() => setDelivery("local")} /><span><strong>Keep it local</strong><small>Changes stay in a saved working folder on this Mac. Nothing is pushed to GitHub.</small></span></label>
          <label className={publishing ? "selected" : ""}><input type="radio" name="delivery" value="publish" disabled={!publishable} checked={publishing} onChange={() => setDelivery("publish")} /><span><strong>Open a pull request</strong><small>{publishable ? "Jig commits the changes, pushes a branch, and opens a pull request with your GitHub login." : "Needs a GitHub project, such as github.com/owner/repo. A local folder can only keep changes local."}</small></span></label>
        </fieldset>}
        {continueID && <div className="notice continuation" role="status">
          {continuation ? <><strong>Continuing:</strong> {continuation.title.split("\n")[0]}
            <p className="subtle">{continuation.ref ? `Starts from the earlier task's branch ${continuation.ref}, so its changes are included.` : continuation.keptLocal ? "The earlier changes were kept local and never pushed, so this task starts from the project's latest commit without them. The earlier directive and result are passed along as context." : "This task starts from the project's latest commit. The earlier directive and result are passed along as context."}</p></> : "Loading the earlier task…"}
        </div>}
        <details className="goal-options" open={mode !== "auto"}><summary>{mode === "auto" ? "Add a goal or limits (optional)" : "Your directive"}</summary>
        <label htmlFor="task">{mode === "auto" ? "3. Goal or limits (optional)" : "3. What should Codex do?"}</label>
        <textarea id="task" required={mode !== "auto"} rows={5} maxLength={20000} placeholder={mode === "auto" ? "Leave blank: agents inspect the project and choose one useful improvement. Or give a goal, such as make installation easier." : mode === "ask" ? "Explain what this project does and how I can use it." : "Describe the change you want and what a working result looks like."} value={task} onChange={(event) => setTask(event.target.value)} />
        </details>
        <details className="evidence"><summary>Options</summary>
          <label htmlFor="model">Codex model</label>
          <input id="model" required value={model} onChange={(event) => setModel(event.target.value)} />
          {mode !== "ask" && <><label htmlFor="test-command">Test command</label><input id="test-command" placeholder="Leave blank to detect the project's test command" value={testCommand} onChange={(event) => setTestCommand(event.target.value)} /></>}
        </details>
        <ErrorBanner error={error} />
        <ErrorBanner error={fleet.error} />
        <div className="start-row">
          <button className="primary" type="submit" disabled={pending || !codexReady || Boolean(fleet.error) || !repository.trim() || (mode !== "auto" && !task.trim()) || !model.trim() || Boolean(continueID && !continuation)}>{pending ? "Starting…" : "Start task"}</button>
          <span className="subtle">{codexReady ? `Uses your signed-in Codex. ${publishing ? "Passing work becomes a pull request." : "Results stay local."}` : fleet.loading ? "Checking Codex…" : "Codex is offline. Check Workers before starting."}</span>
        </div>
        <p className="task-intent"><strong>{mode === "ask" ? "This task will return an answer." : "This task will change files in a separate working folder."}</strong> {mode === "ask" ? "Choose Make changes if you want Codex to implement something." : publishing ? "Your original project stays untouched; review and merge the pull request on GitHub." : "Your original project stays untouched. Nothing reaches GitHub unless you choose Open a pull request."}</p>
      </fieldset>
    </form>
    <aside className="recent-work" aria-label="Recent work">
    <header className="view-header"><div><span className="eyebrow">IN THE FACTORY</span><h2>Recent tasks</h2></div><Link href="/runs">View all →</Link></header>
    <ErrorBanner error={runs.error} />
    <div className="task-cards">{(runs.data ?? []).slice(0, 6).map((run) => <TaskCard key={run.id} run={run} />)}</div>
    {!runs.loading && runs.data?.length === 0 && <p className="subtle">Your first task will appear here when you press Start.</p>}
    <div className="loop-guide"><span className="eyebrow">THE AGENT LOOP</span><div><span>01</span><strong>Plan</strong><p>Find a useful change. Write the directive.</p></div><div><span>02</span><strong>Build</strong><p>Turn the plan into working code.</p></div><div><span>03</span><strong>Verify</strong><p>Run tests. Replan and repair if they fail.</p></div><p className="subtle">Up to three repair rounds. You choose whether passing work stays local or becomes a pull request.</p></div>
    </aside>
    </div>
  </div>;
}

export function TaskCard({ run }: { run: Run }) {
  const title = taskTitle(run) === automaticGoal ? "Agent-led improvement" : taskTitle(run).split("\n")[0].replace(/^>\s*/, "");
  return <article className={`panel task-card task-card-${taskState(run)}`}>
      <div className="task-card-meta"><span className="task-project">{run.targets.map((target) => projectName(target.repository)).join(", ")}</span><StatusBadge state={taskState(run)} /></div>
      <h3><Link href={`/runs/${run.id}`}>{title}</Link></h3>
      <p className="subtle">{formatDateTime(run.created_at)}</p>
      <Link href={`/runs/${run.id}`}>Open task →</Link>
    </article>;
}
