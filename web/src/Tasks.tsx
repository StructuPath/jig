import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api, ensureDefinition } from "./api";
import { Floor } from "./Floor";
import { formatAge, formatDateTime, projectName, taskHeading, taskState } from "./format";
import { useNow, usePolled, useSharedPoll } from "./polling";
import { useDocumentTitle } from "./keyboard";
import { navigate } from "./router";
import { loadContinuation } from "./continuation";
import type { Continuation } from "./continuation";
import { startHelp, workerCommand } from "./startHelp";
import { automaticGoal, canPublish, defaultModel, starterSource } from "./starters";
import type { Delivery, TaskMode } from "./starters";
import type { JobStatus } from "./status";
import type { Run } from "./types";
import { ErrorBanner, JobBadge, Link, StatusBadge } from "./ui";

// The Floor is the newest work on the line, newest first; the Jobs list holds
// the rest. 30 one-line rows fill a 1080p screen beside the start form.
const FLOOR_ROWS = 30;

function storedProject(): string | null {
  try { return localStorage.getItem("jig-project"); } catch { return null; /* Browser storage can be disabled. */ }
}

// A draft is the form as it stood, kept in browser storage so a refresh or a
// refused start does not cost the operator what they typed. It is dropped
// once a job starts. A link that names a project, mode, or continuation is
// an explicit request and wins over a draft.
interface Draft {
  repository?: string; task?: string; mode?: TaskMode; saved?: boolean; workflowID?: string;
  delivery?: Delivery; model?: string; testCommand?: string;
}
const DRAFT_KEY = "jig-draft";

function readDraft(): Draft {
  const query = new URLSearchParams(window.location.search);
  if (query.has("project") || query.has("mode") || query.has("continue")) return {};
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(DRAFT_KEY) ?? "{}");
    return parsed && typeof parsed === "object" ? parsed as Draft : {};
  } catch { return {}; /* Storage can be disabled, and a stale draft can be unreadable. */ }
}

export function Tasks() {
  const loadRuns = useCallback(() => api.runs(), []);
  const runs = usePolled(loadRuns, 3000, "tasks");
  const fleet = useSharedPoll("workers", api.fleet, 3000);
  const [draft] = useState(readDraft);
  const [repository, setRepository] = useState(() => new URLSearchParams(window.location.search).get("project") ?? (typeof draft.repository === "string" ? draft.repository : null) ?? storedProject() ?? "");
  const [task, setTask] = useState(() => typeof draft.task === "string" ? draft.task : "");
  const [mode, setMode] = useState<TaskMode>(() => { const mode = new URLSearchParams(window.location.search).get("mode") ?? draft.mode; return mode === "ask" || mode === "build" ? mode : "auto"; });
  const [model, setModel] = useState(() => typeof draft.model === "string" && draft.model.trim() ? draft.model : defaultModel);
  const [testCommand, setTestCommand] = useState(() => typeof draft.testCommand === "string" ? draft.testCommand : "");
  const [delivery, setDelivery] = useState<Delivery>(() => draft.delivery === "publish" ? "publish" : "local");
  const [continueID] = useState(() => new URLSearchParams(window.location.search).get("continue"));
  const [continuation, setContinuation] = useState<Continuation | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [saved, setSaved] = useState(() => draft.saved === true);
  const [workflowID, setWorkflowID] = useState(() => typeof draft.workflowID === "string" ? draft.workflowID : "");
  useEffect(() => {
    if (continueID) return;
    try {
      localStorage.setItem(DRAFT_KEY, JSON.stringify({ repository, task, mode, saved, workflowID, delivery, model, testCommand } satisfies Draft));
    } catch { /* The form still works for this visit. */ }
  }, [continueID, repository, task, mode, saved, workflowID, delivery, model, testCommand]);
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
  // The server queues a job whether or not a worker is online, so a missing
  // worker does not block Start: the note under the button says the job
  // waits. Only a saved-workflow choice with no workflow cannot start.
  const ready = saved ? Boolean(workflow) : true;
  const runnable = saved ? (pinned ? liveRuntimes.includes(pinned) : liveRuntimes.length > 0) : codexReady;
  // The command names the runtime the job needs: Codex for the task form's
  // own jobs, and the pinned runtime for a saved workflow that pins one.
  const needed = saved ? pinned : "codex";
  const waitNote = liveRuntimes.length === 0
    ? <>No worker is online — the job waits until you run <code>{workerCommand(needed)}</code>.</>
    : `No online worker runs ${saved ? pinned ?? "any runtime" : "Codex"} — the job waits in the queue until one does.`;
  const taskNeeded = saved || mode !== "auto";
  const nowMs = useNow(1000);
  useDocumentTitle("Floor");

  const start = async (event: FormEvent) => {
    event.preventDefault();
    if (pending || !repository.trim() || (taskNeeded && !task.trim()) || (!saved && !model.trim()) || !ready || fleet.error || (continueID && !continuation)) return;
    setPending(true);
    setError(null);
    try {
      const chosen: Delivery = publishing ? "publish" : "local";
      const result = saved && workflow
        ? await api.startRun(workflow.id, repository.trim(), task.trim(), {
          context: continuation?.context, ref: continuation?.ref, continues: continuation?.runID,
        })
        : await api.startTask((await ensureDefinition(await starterSource(mode, model, testCommand, chosen))).id, repository.trim(), task.trim() || automaticGoal, mode, {
          delivery: chosen, context: continuation?.context, ref: continuation?.ref, continues: continuation?.runID,
        });
      try { localStorage.setItem("jig-project", repository.trim()); localStorage.removeItem(DRAFT_KEY); } catch { /* Task creation succeeded even if browser storage is disabled. */ }
      navigate(`/runs/${result.run.id}`);
    } catch (failure) {
      setError(failure as Error);
    } finally {
      setPending(false);
    }
  };

  const pick = (next: TaskMode) => { setSaved(false); setMode(next); };
  const help = startHelp(error, repository);

  return <div className="view floor-view">
    <section className="floor-main" aria-label="On the line">
      <header className="floor-head"><h1>On the line</h1><Link href="/runs">All jobs →</Link></header>
      <ErrorBanner error={runs.error} />
      {(runs.data ?? []).length > 0 && <Floor runs={(runs.data ?? []).slice(0, FLOOR_ROWS)} nowMs={nowMs} />}
      {!runs.loading && (runs.data ?? []).length === 0 && <div className="floor-empty">
        <p>The line is empty. Fill in <strong>Start a job</strong> and press Start job, or admit a run of a saved workflow from a terminal:</p>
        <pre>jig def invoke &lt;workflow&gt; --repo &lt;path&gt; --instructions "&lt;what to do&gt;"</pre>
      </div>}
      <p className="floor-workers">
        {fleet.data ? (fleet.data.workers ?? []).filter((worker) => worker.live).length === 0 ? <>No worker is online. Jobs wait in the queue until you run <code>{workerCommand("codex")}</code>.</> :
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
            <p className="subtle">{continuation.ref ? `Starts from the earlier job's branch ${continuation.ref}, so its changes are included.` : continuation.keptLocal ? "The earlier changes were kept local and never pushed, so this job starts from the project's latest commit without them. The earlier directive and result are passed along as context." : "This job starts from the project's latest commit. The earlier directive and result are passed along as context."}</p></> : "Loading the earlier job…"}
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
        {help && <div className="start-help" role="alert">
          <strong>{help.title}</strong>
          <p>{help.detail}</p>
          {help.commands.length > 0 && <><p className="subtle">Run this in a terminal, then press Start again. Your text is kept.</p><pre>{help.commands.join("\n")}</pre></>}
        </div>}
        <ErrorBanner error={error} />
        <ErrorBanner error={fleet.error} />
        <ErrorBanner error={definitions.error} />
        <div className="start-row">
          <button className="primary" type="submit" disabled={pending || !ready || Boolean(fleet.error) || !repository.trim() || (taskNeeded && !task.trim()) || (!saved && !model.trim()) || Boolean(continueID && !continuation)}>{pending ? "Starting…" : "Start job"}</button>
          <span className="subtle">{!fleet.data && fleet.loading ? "Checking workers…" : !runnable ? waitNote : saved
            ? `Runs ${workflow?.name ?? "the workflow"} on ${pinned ?? "an online worker"}.`
            : `Uses your signed-in Codex. ${publishing ? "Passing work becomes a pull request." : "Results stay local."}`}</span>
        </div>
        {!saved && <p className="task-intent"><strong>{mode === "ask" ? "This job will return an answer." : "This job will change files in a separate working folder."}</strong> {mode === "ask" ? "Choose Make changes if you want Codex to implement something." : publishing ? "Your original project stays untouched; review and merge the pull request on GitHub." : "Your original project stays untouched. Nothing reaches GitHub unless you choose Open a pull request."}</p>}
      </fieldset>
    </form>
  </div>;
}

export function TaskCard({ run, status, nowMs }: { run: Run; status?: JobStatus; nowMs: number }) {
  const title = taskHeading(run);
  return <article className={`panel task-card task-card-${status ? `tone-${status.tone}` : taskState(run)}`}>
      <div className="task-card-meta"><span className="task-project">{run.targets.map((target) => projectName(target.repository)).join(", ")}</span>{status ? <JobBadge status={status} /> : <StatusBadge state={taskState(run)} />}</div>
      <h3><Link href={`/runs/${run.id}`}>{title}</Link></h3>
      <p className="subtle" title={formatDateTime(run.created_at)}>{formatAge(run.created_at, nowMs)}</p>
      <Link href={`/runs/${run.id}`}>Open job →</Link>
    </article>;
}
