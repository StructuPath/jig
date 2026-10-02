// router.ts — a 40-line history router. The app has six views; a routing
// library would be more dependency than navigation. Paths are real (not
// hash) because the server does SPA fallback for extension-less paths, and
// deep links into a run must survive a reload.
import { useCallback, useEffect, useState } from "react";

export type Route =
  | { name: "tasks" }
  | { name: "queue" }
  | { name: "runs" }
  | { name: "run"; id: string }
  | { name: "job"; id: string }
  | { name: "worktrees" }
  | { name: "fleet" }
  | { name: "unknown"; path: string };

export function parseRoute(path: string): Route {
  const parts = path.split("/").filter(Boolean);
  if (parts.length === 0) return { name: "tasks" };
  if (parts[0] === "queue" && parts.length === 1) return { name: "queue" };
  if (parts[0] === "runs" && parts.length === 1) return { name: "runs" };
  if (parts[0] === "runs" && parts.length === 2) return { name: "run", id: decodeURIComponent(parts[1]) };
  if (parts[0] === "jobs" && parts.length === 2) return { name: "job", id: decodeURIComponent(parts[1]) };
  if (parts[0] === "worktrees" && parts.length === 1) return { name: "worktrees" };
  if (parts[0] === "fleet" && parts.length === 1) return { name: "fleet" };
  return { name: "unknown", path };
}

export function navigate(path: string): void {
  window.history.pushState({}, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

export function useRoute(): Route {
  const [path, setPath] = useState(() => window.location.pathname);
  useEffect(() => {
    const update = () => setPath(window.location.pathname);
    window.addEventListener("popstate", update);
    return () => window.removeEventListener("popstate", update);
  }, []);
  return parseRoute(path);
}

/** useNavigate returns a click handler that keeps the SPA on one document. */
export function useNavigate(): (path: string) => (event: { preventDefault: () => void }) => void {
  return useCallback(
    (path: string) => (event: { preventDefault: () => void }) => {
      event.preventDefault();
      navigate(path);
    },
    [],
  );
}
