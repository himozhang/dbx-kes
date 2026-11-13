import { createPinia, disposePinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { usePlDebugStore } from "@/stores/plDebugStore";

describe("plDebugStore package breakpoints", () => {
  let pinia: ReturnType<typeof createPinia>;

  beforeEach(() => {
    pinia = createPinia();
    setActivePinia(pinia);
  });

  afterEach(() => {
    disposePinia(pinia);
  });

  it("collects pending breakpoints from every member of a package", () => {
    const store = usePlDebugStore();
    store.togglePendingBreakpoint({ schema: "APP", packageName: "DEMO_PKG", objectName: "CREATE_ORDER" }, 5);
    store.togglePendingBreakpoint({ schema: "APP", packageName: "DEMO_PKG", objectName: "FIND_ORDER" }, 12);
    store.togglePendingBreakpoint({ schema: "APP", packageName: "OTHER_PKG", objectName: "RUN" }, 3);

    expect(store.pendingPackageBreakpointLines("APP", "DEMO_PKG").sort((a, b) => a - b)).toEqual([5, 12]);
    expect(store.pendingBreakpointLinesFor({ schema: "APP", packageName: "DEMO_PKG", objectName: "FIND_ORDER" })).toEqual([12]);
  });

  it("removes a breakpoint when the same line is toggled again", () => {
    const store = usePlDebugStore();
    const target = { schema: "APP", packageName: "DEMO_PKG", objectName: "CREATE_ORDER" };
    store.togglePendingBreakpoint(target, 5);
    store.togglePendingBreakpoint(target, 5);
    expect(store.pendingPackageBreakpointLines("APP", "DEMO_PKG")).toEqual([]);
  });
});
