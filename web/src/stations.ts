// stations.ts — the fold from a frozen chain plus one attempt's record to the
// station line: every phase of the definition in declared order, each with a
// state, a wall clock, a spend, and one short note, followed by the delivery
// steps (publish, CI) the job's policy adds. Pure, so the line is testable
// without a DOM.
//
// The chain comes from the job detail's `stations` (the run's frozen
// snapshot, parsed by the control plane). When it is absent — an older
// control plane, an unreadable snapshot — the line falls back to the phases
// the trace has seen so far, which is what the page showed before.
import { parseAttemptSummary } from "./api";
import { isTerminalAttempt, parseTime } from "./format";
import type { Lane } from "./lanes";
import type { Attempt, AttemptSummary, Delivery, PhaseResult, PublishRecord, Station, TraceEvent } from "./types";

export type StationState = "pass" | "live" | "wait" | "skip" | "bad" | "held";

export interface StationView {
  /** The phase name, or "publish" / "ci" for the delivery steps. */
  name: string;
  label: string;
  kind: "agent" | "code" | "delivery";
  state: StationState;
  /** Wall clock across every round; null when the station has not run. */
  durationMs: number | null;
  /** Agent spend across every round; null when none was recorded. */
  costUSD: number | null;
  /** How many times the station ran (repair loops re-run a station). */
  rounds: number;
  note: string;
}

/** stationLabel shortens a phase name to what fits under a lamp. */
export function stationLabel(name: string): string {
  const short: Record<string, string> = {
    ci: "CI", scout: "read", "classify-risk": "risk", "review-risk": "risk review",
    "review-correctness": "correct", "review-security": "secure", "review-maintainability": "maintain",
    "commit-build": "commit", "commit-revision": "recommit",
  };
  return short[name] ?? name.replace(/^review-/, "").replace(/-/g, " ");
}

/**
 * agentReport reads an agent's closing envelope out of its last text. The
 * CLI prints the envelope as its final message, and a feed that shows raw
 * JSON is a feed nobody reads: the summary is what the agent said.
 */
function agentReport(text: string): string | null {
  if (!text.startsWith("{")) return null;
  try {
    const parsed: unknown = JSON.parse(text);
    const summary = parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>).summary : undefined;
    return typeof summary === "string" ? summary.split("\n")[0] : null;
  } catch {
    return null;
  }
}

function envelopeFields(phase: PhaseResult | undefined): Record<string, unknown> {
  const envelope = phase?.envelope;
  return envelope && typeof envelope === "object" ? (envelope as Record<string, unknown>) : {};
}

/** holdField names the envelope field a `hold_when` predicate reads. */
function holdField(delivery: Delivery | undefined): string | null {
  const match = /^\s*([A-Za-z_][\w.]*)\s*(==|!=)/.exec(delivery?.hold_when ?? "");
  return match ? match[1] : null;
}

function publishState(summary: AttemptSummary | null): { state?: string; code?: string } {
  const publish = summary?.publish;
  if (!publish) return {};
  return typeof publish === "string" ? { state: publish } : { state: publish.state, code: publish.code };
}

/** isLocalDelivery is the task form's own hold: work kept local on purpose. */
function isLocalDelivery(delivery: Delivery | undefined): boolean {
  return (delivery?.hold_when ?? "").includes("jig_ui_delivery");
}

function pullRequestNumber(url: string | undefined): string | null {
  const match = /\/pull\/(\d+)/.exec(url ?? "");
  return match ? `#${match[1]}` : null;
}

export function buildStations(
  chain: Station[] | undefined,
  delivery: Delivery | undefined,
  attempt: Attempt,
  lane: Lane,
  events: TraceEvent[],
  publish: PublishRecord[],
  nowMs: number,
): StationView[] {
  const summary = parseAttemptSummary(attempt.result);
  const recorded = summary?.phases ?? [];
  const terminal = isTerminalAttempt(attempt.state);
  const held = Boolean(summary?.publish_hold) && !isLocalDelivery(delivery);
  const heldBy = held ? holdField(delivery) : null;
  const names: Station[] = chain?.length
    ? chain
    : [...new Set(lane.phases.map((span) => span.phase))].map((name) => ({ name, kind: "agent" }));

  const spend = new Map<string, number>();
  for (const event of events) {
    if (event.type !== "agent_end" || !event.phase) continue;
    const payload = event.payload;
    const cost = payload && typeof payload === "object" ? (payload as Record<string, unknown>).cost : undefined;
    if (typeof cost === "number" && cost > 0) spend.set(event.phase, (spend.get(event.phase) ?? 0) + cost);
  }

  const views: StationView[] = names.map((station) => {
    const spans = lane.phases.filter((span) => span.phase === station.name);
    const last = spans.at(-1);
    const result = recorded.slice().reverse().find((phase) => phase.phase === station.name);
    const fields = envelopeFields(result);
    const status = result?.status ?? last?.status ?? (last && last.endMs === null ? "running" : undefined);

    let state: StationState = "wait";
    if (status === "running") state = terminal ? "bad" : "live";
    else if (status === "success") state = "pass";
    else if (status === "skipped") state = "skip";
    else if (status !== undefined) state = "bad";
    if (state === "pass" && heldBy && heldBy in fields) state = "held";

    let durationMs: number | null = null;
    if (spans.length > 0) {
      durationMs = spans.reduce((total, span) => total + Math.max(0, (span.endMs ?? nowMs) - span.startMs), 0);
    } else {
      const started = parseTime(result?.started_at);
      const ended = parseTime(result?.ended_at);
      if (started !== null && ended !== null) durationMs = Math.max(0, ended - started);
    }

    let note = station.kind === "agent" ? station.model ?? "" : "check";
    if (state === "wait") note = terminal ? "not run" : station.if ? `if ${station.if}` : "waiting";
    else if (state === "skip") note = "not required";
    else if (state === "live") note = station.model ? `${station.model} · running` : "running";
    else if (state === "bad") note = status === "death" ? "died" : status === "fail" ? "failed" : "stopped";
    else if (typeof fields.approved === "boolean") note = fields.approved ? "approved" : "rejected";
    else if (typeof fields.risk === "string") note = `risk ${fields.risk}`;

    return {
      name: station.name,
      label: stationLabel(station.name),
      kind: station.kind === "code" ? "code" : "agent",
      state,
      durationMs,
      costUSD: spend.get(station.name) ?? null,
      rounds: Math.max(spans.length, result ? 1 : 0),
      note,
    };
  });

  // Delivery steps exist only for a job that has a publish policy on record;
  // a direct `jig run` never publishes and shows none.
  if (!delivery) return views;

  const mine = publish.filter((record) => record.attempt_id === attempt.id);
  const step = (name: string) => mine.find((record) => record.step === name);
  const proof = step("proof");
  const ci = step("ci");
  const chainDone = views.length > 0 && views.every((view) => view.state === "pass" || view.state === "skip" || view.state === "held");
  const chainEnd = Math.max(0, ...lane.phases.map((span) => span.endMs ?? 0));
  const outcome = publishState(summary);
  const proofAt = parseTime(proof?.completed_at);
  const ciAt = parseTime(ci?.completed_at);
  const pr = pullRequestNumber(mine.find((record) => record.pr_url)?.pr_url);

  const publishView: StationView = {
    name: "publish", label: "publish", kind: "delivery", state: "wait",
    durationMs: null, costUSD: null, rounds: mine.length > 0 ? 1 : 0, note: terminal ? "not run" : "waiting",
  };
  if (proof) {
    publishView.state = "pass";
    publishView.note = pr ?? "ref proven";
    if (proofAt !== null && chainEnd > 0) publishView.durationMs = Math.max(0, proofAt - chainEnd);
  } else if (outcome.state === "held") {
    publishView.state = isLocalDelivery(delivery) ? "skip" : "held";
    publishView.note = isLocalDelivery(delivery) ? "kept local" : "held";
  } else if (outcome.code === "publish_empty_changeset") {
    publishView.state = "skip";
    publishView.note = "nothing to publish";
  } else if (outcome.state === "not_attempted") {
    publishView.state = "skip";
    publishView.note = "not attempted";
  } else if (terminal && chainDone && attempt.state === "accepted_unpublished") {
    publishView.state = "bad";
    publishView.note = outcome.code ?? "failed";
  } else if (!terminal && chainDone) {
    publishView.state = "live";
    publishView.note = "pushing";
    if (chainEnd > 0) publishView.durationMs = Math.max(0, nowMs - chainEnd);
  }
  views.push(publishView);

  if (delivery.ci_wait) {
    const ciView: StationView = {
      name: "ci", label: "CI", kind: "delivery", state: "wait",
      durationMs: null, costUSD: null, rounds: ci ? 1 : 0,
      note: publishView.state === "held" ? "held" : terminal ? "not run" : "waiting",
    };
    if (ci) {
      ciView.state = "pass";
      ciView.note = "green";
      if (ciAt !== null && proofAt !== null) ciView.durationMs = Math.max(0, ciAt - proofAt);
    } else if (proof && !terminal) {
      ciView.state = "live";
      ciView.note = "checks running";
      if (proofAt !== null) ciView.durationMs = Math.max(0, nowMs - proofAt);
    } else if (proof && terminal && (outcome.code ?? "").startsWith("ci_")) {
      ciView.state = "bad";
      ciView.note = outcome.code ?? "red";
    }
    views.push(ciView);
  }
  return views;
}

export interface Verdict {
  word: string;
  tone: "good" | "live" | "bad" | "idle";
}

/** verdictFor is the one word the job page leads with. */
export function verdictFor(attempt: Attempt, stations: StationView[], delivery: Delivery | undefined): Verdict {
  const summary = parseAttemptSummary(attempt.result);
  const outcome = publishState(summary);
  switch (attempt.state) {
    case "queued": return { word: "Queued", tone: "idle" };
    case "preparing": return { word: "Preparing", tone: "live" };
    case "running": {
      const live = stations.find((station) => station.state === "live");
      return { word: live ? live.label : "Running", tone: "live" };
    }
    case "accepted": return { word: "Accepted", tone: "good" };
    case "accepted_unpublished":
      if (outcome.state === "held") return isLocalDelivery(delivery) ? { word: "Ready", tone: "good" } : { word: "Held", tone: "bad" };
      if (outcome.code === "publish_empty_changeset" || outcome.state === "not_attempted") return { word: "Done", tone: "good" };
      return { word: "Unpublished", tone: "bad" };
    case "cancelled": return { word: "Cancelled", tone: "idle" };
    case "lost": return { word: "Lost", tone: "bad" };
    default: return { word: "Stopped", tone: "bad" };
  }
}

export interface FeedLine {
  seq: number;
  at?: string;
  verb: string;
  detail: string;
}

function payloadString(event: TraceEvent, ...fields: string[]): string {
  const payload = event.payload;
  if (!payload || typeof payload !== "object") return "";
  for (const field of fields) {
    const value = (payload as Record<string, unknown>)[field];
    if (typeof value === "string" && value.trim() !== "") return value.trim().split("\n")[0];
  }
  return "";
}

/**
 * buildFeed turns the trace's tail into what an operator reads: the tool and
 * what it was pointed at, not "the agent used a tool". Delivery bookkeeping
 * phases the task form adds are left out.
 */
export function buildFeed(events: TraceEvent[], limit = 8): FeedLine[] {
  const lines: FeedLine[] = [];
  for (const event of events) {
    if (["keep-local", "deliver"].includes(event.phase ?? "")) continue;
    const base = { seq: event.seq, at: event.started_at };
    if (event.type === "tool_call") {
      lines.push({ ...base, verb: event.name ?? "tool",
        detail: payloadString(event, "command", "file_path", "path", "pattern", "query", "url", "description") });
    } else if (event.type === "log" && event.name === "agent_text") {
      const payload = event.payload;
      const whole = payload && typeof payload === "object" ? (payload as Record<string, unknown>).text : undefined;
      const report = typeof whole === "string" ? agentReport(whole.trim()) : null;
      lines.push(report !== null
        ? { ...base, verb: "report", detail: report }
        : { ...base, verb: "say", detail: payloadString(event, "text") });
    } else if (event.type === "log" && event.name === "repair_edge") {
      lines.push({ ...base, verb: "repair", detail: "Check failed → agents are repairing and retesting automatically" });
    } else if (event.type === "log" && event.name === "repair_exhausted") {
      lines.push({ ...base, verb: "repair", detail: "Repair limit reached; see the diagnostic" });
    } else if (event.type === "phase_start") {
      lines.push({ ...base, verb: "start", detail: stationLabel(event.phase ?? "") });
    } else if (event.type === "phase_end") {
      lines.push({ ...base, verb: "done", detail: `${stationLabel(event.phase ?? "")} · ${payloadString(event, "status") || "ended"}` });
    } else if (event.type === "gate_fail") {
      lines.push({ ...base, verb: "gate", detail: `${event.name ?? "gate"} failed in ${stationLabel(event.phase ?? "")}` });
    } else if (event.type === "error" || event.type === "phase_death") {
      lines.push({ ...base, verb: "error", detail: payloadString(event, "error", "reason", "cause") || (event.name ?? "worker reported an error") });
    }
  }
  return lines.slice(-limit);
}
