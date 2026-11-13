// @vitest-environment happy-dom
import { createApp, h, nextTick, type App } from "vue";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, expect, it, vi } from "vitest";
import i18n from "@/i18n";
import { useConnectionStore } from "@/stores/connectionStore";
import { useQueryStore } from "@/stores/queryStore";
import type { ConnectionConfig } from "@/types/database";
import ContentArea from "../ContentArea.vue";

const rpc = vi.hoisted(() => ({
  getObjectSource: vi.fn(async () => undefined),
  executeQuery: vi.fn(),
  listCompletionObjects: vi.fn(async () => []),
  listPlugins: vi.fn(async () => []),
  listInstalledAgents: vi.fn(async () => []),
  loadSchemaCache: vi.fn(async () => null),
  saveSchemaCache: vi.fn(async () => undefined),
  deleteSchemaCachePrefix: vi.fn(async () => undefined),
  checkConnectionHealth: vi.fn(async () => undefined),
  saveOpenTabsState: vi.fn(async () => undefined),
}));
vi.mock("@/lib/backend/api", async (original) => ({ ...(await original<typeof import("@/lib/backend/api")>()), ...rpc }));
let app: App | undefined;
afterEach(() => {
  app?.unmount();
  app = undefined;
  vi.restoreAllMocks();
  document.body.innerHTML = "";
});

const header = `CREATE OR REPLACE PACKAGE "public"."test_pkg" AS
PROCEDURE existing(p_id INTEGER);
END test_pkg;`;
const body = `CREATE OR REPLACE PACKAGE BODY "public"."test_pkg" AS
PROCEDURE existing(p_id INTEGER) IS
BEGIN
  NULL;
END existing;
END test_pkg;`;

it("creates and deletes package members from the header outline and syncs the body", async () => {
  vi.clearAllMocks();
  const pinia = createPinia();
  setActivePinia(pinia);
  i18n.global.locale.value = "en";
  const config = { id: "content-package", name: "KES", db_type: "kingbase", username: "system", database: "test" } as ConnectionConfig;
  const connection = useConnectionStore();
  connection.connections = [config];
  connection.connectedIds.add(config.id);
  const query = useQueryStore();
  const id = query.createTab(config.id, "test", "test_pkg", "query", "public", body, undefined, { forceNew: true });
  const tab = query.tabs.find((candidate) => candidate.id === id)!;
  tab.sourceView = true;
  tab.objectSource = { name: "test_pkg", schema: "public", objectType: "PACKAGE" };
  tab.packageHeaderSource = header;
  query.updateSql(id, body);
  const host = document.createElement("div");
  document.body.append(host);
  app = createApp({
    render: () =>
      h(ContentArea, {
        activeTab: tab,
        activeConnection: config,
        activeOutputView: "result",
        executableSql: tab.sql,
        formatSqlRequest: null,
        compressSqlRequest: null,
        selectedSql: "",
        cursorPos: 0,
        blockDangerousRedisCommands: false,
        editorOnly: true,
        onEditorUpdate: (tabId: string, sql: string) => query.updateSql(tabId, sql),
      }),
  });
  app.use(pinia);
  app.use(i18n);
  app.mount(host);

  const outline = () => host.querySelector<HTMLElement>("[data-package-outline-table]");
  await vi.waitFor(() => expect(outline()).not.toBeNull(), { timeout: 10_000 });
  expect(outline()!.textContent).toContain("existing");

  // 新建子对象：包头声明与包体实现同时写入，左侧大纲立即显示新成员。
  host.querySelector<HTMLButtonElement>("[data-package-outline-create]")!.click();
  await nextTick();
  const input = document.body.querySelector<HTMLInputElement>("[data-package-member-name-input]")!;
  expect(input).not.toBeNull();
  input.value = "added";
  input.dispatchEvent(new Event("input"));
  await nextTick();
  document.body.querySelector<HTMLButtonElement>("[data-package-member-create-confirm]")!.click();
  await nextTick();

  expect(tab.packageHeaderSource).toContain("PROCEDURE added");
  expect(tab.sql).toContain("PROCEDURE added");
  expect(outline()!.textContent).toContain("added");

  // 删除子对象：包头声明与包体实现同时移除。
  const addedRow = [...host.querySelectorAll<HTMLElement>("[data-package-outline-table] tr")].find((row) => row.textContent?.includes("added"))!;
  expect(addedRow).toBeDefined();
  [...addedRow.querySelectorAll<HTMLButtonElement>("button")].at(-1)!.click();
  await nextTick();
  document.body.querySelector<HTMLButtonElement>("[data-package-member-delete-confirm]")!.click();
  await nextTick();

  expect(tab.packageHeaderSource).not.toContain("added");
  expect(tab.sql).not.toContain("added");
  expect(outline()!.textContent).not.toContain("added");
});
