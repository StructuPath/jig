// ui.tsx — the shared primitives every view uses. Deliberately tiny: state
// colour, an error banner that names the control plane's own code, and a link
// that keeps the SPA on one document.
import type { ReactNode } from "react";
import { APIError } from "./api";
import { stateLabel } from "./format";
import { navigate } from "./router";

export function StatusBadge({ state }: { state: string }) {
  return (
    <span className={`badge badge-${state}`}>
      <span className="dot" />
      {stateLabel(state)}
    </span>
  );
}

export function Link({ href, children, current = false }: { href: string; children: ReactNode; current?: boolean }) {
  return (
    <a
      href={href}
      aria-current={current ? "page" : undefined}
      onClick={(event) => {
        if (event.metaKey || event.ctrlKey || event.shiftKey) return;
        event.preventDefault();
        navigate(href);
      }}
    >
      {children}
    </a>
  );
}

export function ErrorBanner({ error }: { error: Error | null }) {
  if (!error) return null;
  const code = error instanceof APIError ? error.code : "request_failed";
  return (
    <div className="error-banner" role="alert">
      <strong>{code}</strong> {error.message}
    </div>
  );
}

export function Empty({ title, detail }: { title: string; detail: string }) {
  return (
    <div className="empty">
      <h2>{title}</h2>
      <p>{detail}</p>
    </div>
  );
}

export function Loading({ label }: { label: string }) {
  return (
    <div className="loading" role="status">
      {label}
    </div>
  );
}
