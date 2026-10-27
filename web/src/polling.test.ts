import { act, renderHook } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { usePolled } from "./polling";

afterEach(() => vi.useRealTimers());

it("waits for a slow request before polling again", async () => {
  vi.useFakeTimers();
  let finish!: (value: string) => void;
  const load = vi.fn(() => new Promise<string>((resolve) => { finish = resolve; }));
  const { result } = renderHook(() => usePolled(load, 1000, "slow"));
  await act(async () => { vi.advanceTimersByTime(3000); });
  expect(load).toHaveBeenCalledTimes(1);
  await act(async () => { finish("first result"); });
  expect(result.current.data).toBe("first result");
  await act(async () => { vi.advanceTimersByTime(1000); });
  expect(load).toHaveBeenCalledTimes(2);
  await act(async () => { finish("new result"); });
  expect(result.current.data).toBe("new result");
});

it("resumes polling after a failed request", async () => {
  vi.useFakeTimers();
  const failure = new Error("offline");
  const load = vi.fn().mockRejectedValueOnce(failure).mockResolvedValueOnce("recovered");
  const { result } = renderHook(() => usePolled<string>(load, 1000, "recovery"));
  await act(async () => {});
  expect(result.current.error).toBe(failure);
  await act(async () => { vi.advanceTimersByTime(1000); });
  expect(result.current.data).toBe("recovered");
  expect(result.current.error).toBeNull();
});

it("ignores a pending response after changing the resource", async () => {
  let finishOld!: (value: string) => void;
  const old = () => new Promise<string>((resolve) => { finishOld = resolve; });
  const current = async () => "current resource";
  const { result, rerender } = renderHook(({ key, load }) => usePolled(load, 0, key), {
    initialProps: { key: "old", load: old },
  });
  rerender({ key: "current", load: current });
  await act(async () => {});
  await act(async () => { finishOld("old resource"); });
  expect(result.current.data).toBe("current resource");
});
