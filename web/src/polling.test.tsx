import { act, render, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { usePolled, useSharedPoll } from "./polling";

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

describe("useSharedPoll", () => {
  it("serves every reader of a key from one request and one timer", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    const load = async () => ++calls;
    function Reader({ every }: { every: number }) {
      const { data } = useSharedPoll("shared-test", load, every);
      return <span>{data ?? "…"}</span>;
    }
    const view = render(<><Reader every={5000} /><Reader every={3000} /></>);
    await act(() => vi.advanceTimersByTimeAsync(50));
    expect(calls).toBe(1);
    expect(view.container.textContent).toBe("11");
    // The poll runs at the shortest interval any reader asked for.
    await act(() => vi.advanceTimersByTimeAsync(3000));
    expect(calls).toBe(2);
    view.unmount();
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(calls).toBe(2);
    vi.useRealTimers();
  });
});
