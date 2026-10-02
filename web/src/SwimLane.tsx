// SwimLane.tsx — one attempt, one lane, on a time axis (R15). Phases are the
// blocks; tool calls are folded rows under them with a real wall-clock span,
// visible while they are still running and long before the phase has an
// envelope. Gate and error marks sit on the same axis, so "the gate failed
// here, 40 seconds in" is one glance rather than a log search.
import { formatClock, formatDuration } from "./format";
import { laneEnd, offsetPercent, spanDuration, widthPercent } from "./lanes";
import type { Lane, Mark, Span } from "./lanes";
import { StatusBadge } from "./ui";

export interface SwimLaneProps {
  lane: Lane;
  /** now, injected so a live lane's durations are deterministic under test. */
  nowMs: number;
  onSelectPhase?: (phase: string) => void;
}

export function SwimLane({ lane, nowMs, onSelectPhase }: SwimLaneProps) {
  const end = laneEnd(lane, nowMs);
  const total = Math.max(1, end - lane.startMs);
  const live = lane.endMs === null;

  return (
    <section className="lane" aria-label={`attempt ${lane.attemptNumber} swim lane`}>
      <header className="lane-header">
        <h3>Attempt {lane.attemptNumber}</h3>
        <StatusBadge state={lane.state} />
        <span className="lane-meta">
          {formatDuration(total)} · {lane.eventCount} events
          {live ? " · live" : ""}
        </span>
      </header>

      {lane.phases.length === 0 && lane.tools.length === 0 ? (
        <p className="lane-empty">No trace events for this attempt yet.</p>
      ) : (
        <>
          <div className="lane-track" role="list" aria-label="phases">
            {lane.phases.map((span) => (
              <PhaseBlock
                key={span.key}
                span={span}
                startMs={lane.startMs}
                endMs={end}
                nowMs={nowMs}
                onSelect={onSelectPhase}
              />
            ))}
            {lane.marks.map((mark) => (
              <MarkPin key={mark.key} mark={mark} startMs={lane.startMs} endMs={end} />
            ))}
          </div>

          {lane.tools.length > 0 && (
            <details className="evidence" open={live}>
              <summary>Tool activity · {lane.tools.length} calls</summary>
            <ol className="tool-list" aria-label={`attempt ${lane.attemptNumber} tool calls`}>
              {lane.tools.map((span) => (
                <ToolRow
                  key={span.key}
                  span={span}
                  startMs={lane.startMs}
                  endMs={end}
                  nowMs={nowMs}
                />
              ))}
            </ol>
            </details>
          )}
        </>
      )}
    </section>
  );
}

function PhaseBlock({
  span,
  startMs,
  endMs,
  nowMs,
  onSelect,
}: {
  span: Span;
  startMs: number;
  endMs: number;
  nowMs: number;
  onSelect?: (phase: string) => void;
}) {
  const duration = formatDuration(spanDuration(span, nowMs));
  const status = span.status ?? "running";
  return (
    <button
      type="button"
      role="listitem"
      className={`phase-block phase-${status}`}
      style={{
        left: `${offsetPercent(span, startMs, endMs)}%`,
        width: `${widthPercent(span, startMs, endMs, nowMs)}%`,
      }}
      aria-label={`phase ${span.label}, ${status}, ${duration}`}
      onClick={() => onSelect?.(span.phase)}
    >
      <span className="phase-name">{span.label}</span>
      <span className="phase-duration">{duration}</span>
    </button>
  );
}

function MarkPin({ mark, startMs, endMs }: { mark: Mark; startMs: number; endMs: number }) {
  const total = Math.max(1, endMs - startMs);
  const left = Math.min(100, Math.max(0, ((mark.atMs - startMs) / total) * 100));
  return (
    <span
      className={`mark mark-${mark.kind}`}
      style={{ left: `${left}%` }}
      title={`${mark.label}${mark.detail ? `: ${mark.detail}` : ""} (${formatClock(
        new Date(mark.atMs).toISOString(),
      )})`}
      aria-label={`${mark.kind.replace("_", " ")} ${mark.label}`}
    />
  );
}

function ToolRow({
  span,
  startMs,
  endMs,
  nowMs,
}: {
  span: Span;
  startMs: number;
  endMs: number;
  nowMs: number;
}) {
  const running = span.endMs === null;
  const duration = formatDuration(spanDuration(span, nowMs));
  return (
    <li className="tool-row">
      <span className="tool-name">{span.label}</span>
      <span className="tool-track">
        <span
          className={running ? "tool-span tool-running" : "tool-span"}
          style={{
            marginLeft: `${offsetPercent(span, startMs, endMs)}%`,
            width: `${widthPercent(span, startMs, endMs, nowMs)}%`,
          }}
        />
      </span>
      <span className="tool-duration">
        {duration}
        {running ? " (running)" : ""}
      </span>
    </li>
  );
}
