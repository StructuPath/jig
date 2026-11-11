// StationLine.tsx — a job as a production line: every station of the frozen
// chain on one track, lit by what it did. The whole line is visible before
// the first station starts, so "what is still ahead of this change" is never
// a guess, and a held or stopped job shows where it stopped without a click.
import { formatStopwatch, formatUSD } from "./format";
import type { StationView } from "./stations";

const stateWord: Record<StationView["state"], string> = {
  pass: "passed", live: "running", wait: "waiting", skip: "skipped", bad: "stopped", held: "held",
};

export function StationLine({
  stations,
  selected,
  onSelect,
  moving,
  label,
}: {
  stations: StationView[];
  selected: string | null;
  onSelect: (name: string) => void;
  /** The belt moves only while the attempt is live. */
  moving: boolean;
  label: string;
}) {
  return (
    <ol className={`line${moving ? " line-moving" : ""}`} aria-label={label}>
      {stations.map((station, index) => (
        <li key={station.name} className={`station station-${station.state}`}>
          <button
            type="button"
            aria-pressed={selected === station.name}
            aria-label={`station ${index + 1}, ${station.label}, ${stateWord[station.state]}`}
            onClick={() => onSelect(station.name)}
          >
            <span className="lamp" aria-hidden="true">{String(index + 1).padStart(2, "0")}</span>
            <strong>{station.label}{station.rounds > 1 ? ` ×${station.rounds}` : ""}</strong>
            <span className="station-time">{station.durationMs === null ? "—" : formatStopwatch(station.durationMs)}</span>
            <span className="station-note">{station.note}{station.costUSD !== null ? ` · ${formatUSD(station.costUSD)}` : ""}</span>
          </button>
        </li>
      ))}
    </ol>
  );
}

/** TimingStrip is where the time went: one proportional block per station. */
export function TimingStrip({ stations }: { stations: StationView[] }) {
  const timed = stations.filter((station) => (station.durationMs ?? 0) > 0);
  if (timed.length === 0) return null;
  const sum = (kind: StationView["kind"]) =>
    timed.filter((station) => station.kind === kind).reduce((total, station) => total + (station.durationMs ?? 0), 0);
  const parts = [
    sum("agent") > 0 ? `agents ${formatStopwatch(sum("agent"))}` : "",
    sum("code") > 0 ? `checks ${formatStopwatch(sum("code"))}` : "",
    sum("delivery") > 0 ? `delivery ${formatStopwatch(sum("delivery"))}` : "",
  ].filter(Boolean);
  const cost = stations.reduce((total, station) => total + (station.costUSD ?? 0), 0);
  return (
    <div className="timing">
      <div className="timing-strip" aria-hidden="true">
        {timed.map((station) => (
          <span
            key={station.name}
            className={`timing-${station.kind} timing-${station.state}`}
            style={{ flexGrow: station.durationMs ?? 0 }}
            title={`${station.label} ${formatStopwatch(station.durationMs)}`}
          />
        ))}
      </div>
      <p className="timing-key">
        <span>{parts.join(" · ")}</span>
        <span>{cost > 0 ? `agent spend ${formatUSD(cost)}` : ""}</span>
      </p>
    </div>
  );
}
