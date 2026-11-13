import { ref } from "vue";
import { defineStore } from "pinia";
import { effectiveDatabaseTypeForConnection } from "@/lib/database/jdbcDialect";
import { supportsPlsqlTestCases } from "@/lib/database/databaseObjectCapabilities";
import { executeQuery } from "@/lib/backend/tauri";
import { useConnectionStore } from "@/stores/connectionStore";

export interface PlsqlTestCase {
  id?: number;
  case_name: string;
  schema_name: string;
  object_name: string;
  object_type: string;
  package_name?: string | null;
  pre_sql?: string | null;
  call_sql: string;
  params_json: string;
  expected_return?: string | null;
  note?: string | null;
  creator?: string;
  created_at?: string;
  updated_at?: string;
}

export interface PlsqlTestCaseParam {
  name: string;
  direction: "IN" | "OUT" | "INOUT";
  type: string;
  value: string | null;
}

const TC_SCHEMA = "public";
const TC_TABLE = "dbx_plsql_test_cases";
const TC_FQN = `"${TC_SCHEMA}"."${TC_TABLE}"`;

/** Convert a positional row array into a column-keyed object using `columns`. */
function rowToObject(columns: string[], row: (string | number | boolean | null)[]): Record<string, string | number | boolean | null> {
  const obj: Record<string, string | number | boolean | null> = {};
  columns.forEach((col, i) => {
    obj[col] = row[i] ?? null;
  });
  return obj;
}

function rowToTc(columns: string[], row: (string | number | boolean | null)[]): PlsqlTestCase {
  const o = rowToObject(columns, row);
  return {
    id: Number(o.id),
    case_name: String(o.case_name ?? ""),
    schema_name: String(o.schema_name ?? ""),
    object_name: String(o.object_name ?? ""),
    object_type: String(o.object_type ?? ""),
    package_name: o.package_name != null ? String(o.package_name) : null,
    pre_sql: o.pre_sql != null ? String(o.pre_sql) : null,
    call_sql: String(o.call_sql ?? ""),
    params_json: String(o.params_json ?? ""),
    expected_return: o.expected_return != null ? String(o.expected_return) : null,
    note: o.note != null ? String(o.note) : null,
    creator: o.creator != null ? String(o.creator) : undefined,
    created_at: o.created_at != null ? String(o.created_at) : undefined,
    updated_at: o.updated_at != null ? String(o.updated_at) : undefined,
  };
}

function qmarkIdx(i: number): string {
  return `$${i}`;
}

/**
 * Ensure the metadata table exists. Single DDL — safe because CREATE TABLE
 * IF NOT EXISTS is idempotent and the public schema is writable by the
 * current user on KingbaseES Oracle-compat out of the box.
 */
async function ensureTable(connectionId: string, database: string, schema: string): Promise<void> {
  const ddl = `CREATE TABLE IF NOT EXISTS ${TC_FQN} (
    id               BIGSERIAL PRIMARY KEY,
    case_name        TEXT NOT NULL,
    schema_name      TEXT NOT NULL,
    object_name      TEXT NOT NULL,
    object_type      TEXT NOT NULL,
    package_name     TEXT,
    pre_sql          TEXT,
    call_sql         TEXT NOT NULL,
    params_json      TEXT NOT NULL,
    expected_return  TEXT,
    note             TEXT,
    creator          TEXT DEFAULT CURRENT_USER,
    created_at       TIMESTAMPTZ DEFAULT now(),
    updated_at       TIMESTAMPTZ DEFAULT now()
  )`;
  await executeQuery(connectionId, database, ddl, schema);
}

/**
 * Build a CALL SQL skeleton + default parameters for a routine, used both
 * when the user opens a blank test case dialog and when the debug session
 * auto-saves.
 */
export function buildTestCaseSkeleton(opts: { schema: string; objectName: string; objectType: "PROCEDURE" | "FUNCTION"; packageName?: string; signature?: string; params?: PlsqlTestCaseParam[] }): Pick<PlsqlTestCase, "call_sql" | "params_json"> {
  const params = opts.params ?? [];
  const pkgPart = opts.packageName ? `${opts.schema}.${opts.packageName}.${opts.objectName}` : `${opts.schema}.${opts.objectName}`;
  const isFunction = opts.objectType === "FUNCTION";
  const inParts = params.filter((p) => p.direction === "IN" || p.direction === "INOUT").map((p, i) => p.value ?? (p.type?.toUpperCase().startsWith("INT") || p.type?.toUpperCase() === "NUMBER" ? `:p${i + 1}` : `'${p.type ?? "TEXT"}_value'`));
  const callSql = isFunction ? `DECLARE\n  result ${params[0]?.type ?? "NUMBER"};\nBEGIN\n  result := ${pkgPart}(${inParts.join(", ")});\nEND;` : `BEGIN\n  ${pkgPart}(${inParts.join(", ")});\nEND;`;
  return { call_sql: callSql, params_json: JSON.stringify(params) };
}

export const usePlsqlTestCaseStore = defineStore("plsqlTestCase", () => {
  const currentItems = ref<PlsqlTestCase[]>([]);
  const currentGroupKey = ref<string>("");
  const loading = ref(false);
  const error = ref<string | null>(null);

  function groupKey(tc: PlsqlTestCase): string {
    return tc.object_type || "OTHER";
  }

  /** Tree-view groups keyed by object_type, then each case as a leaf. */
  function groupedItems(): Record<string, PlsqlTestCase[]> {
    const groups: Record<string, PlsqlTestCase[]> = {};
    for (const tc of currentItems.value) {
      const k = groupKey(tc);
      (groups[k] ??= []).push(tc);
    }
    // Sort each group by updated_at desc so the newest test case sits on top.
    for (const k of Object.keys(groups)) {
      groups[k].sort((a, b) => (b.updated_at ?? "").localeCompare(a.updated_at ?? ""));
    }
    return groups;
  }

  async function listForObject(connectionId: string, database: string, schema: string, objectName: string, objectType: string, packageName?: string | null) {
    loading.value = true;
    error.value = null;
    try {
      const config = useConnectionStore().getConfig(connectionId);
      const dbType = effectiveDatabaseTypeForConnection(config);
      if (!supportsPlsqlTestCases(dbType)) {
        error.value = "测试用例持久化仅支持 KingbaseES Oracle 兼容模式";
        currentItems.value = [];
        return;
      }
      await ensureTable(connectionId, database, schema);
      const where = `schema_name = ${qmarkIdx(1)} AND object_name = ${qmarkIdx(2)} AND object_type = ${qmarkIdx(3)}`;
      const args: any[] = [schema, objectName, objectType];
      let extraWhere = "";
      if (packageName) {
        extraWhere = ` AND (package_name = ${qmarkIdx(4)} OR package_name IS NULL)`;
        args.push(packageName);
      }
      const sql = `SELECT * FROM ${TC_FQN} WHERE ${where}${extraWhere} ORDER BY updated_at DESC, id DESC LIMIT 1000`;
      const result = await executeQuery(connectionId, database, sql, TC_SCHEMA);
      currentItems.value = (result.rows ?? []).map((row) => rowToTc(result.columns, row));
      currentGroupKey.value = `${schema}.${objectName}.${objectType}${packageName ? `.${packageName}` : ""}`;
    } catch (e: any) {
      error.value = e?.message ?? String(e);
      currentItems.value = [];
    } finally {
      loading.value = false;
    }
  }

  async function saveTestCase(connectionId: string, database: string, tc: PlsqlTestCase): Promise<number | null> {
    error.value = null;
    try {
      await ensureTable(connectionId, database, tc.schema_name);
      if (tc.id && tc.id > 0) {
        const sql = `UPDATE ${TC_FQN} SET
          case_name=${qmarkIdx(1)}, schema_name=${qmarkIdx(2)}, object_name=${qmarkIdx(3)}, object_type=${qmarkIdx(4)},
          package_name=${qmarkIdx(5)}, pre_sql=${qmarkIdx(6)}, call_sql=${qmarkIdx(7)}, params_json=${qmarkIdx(8)},
          expected_return=${qmarkIdx(9)}, note=${qmarkIdx(10)}, updated_at=now()
          WHERE id=${qmarkIdx(11)} RETURNING id`;
        const result = await executeQuery(connectionId, database, sql, TC_SCHEMA, undefined, { maxRows: 1 });
        const idIdx = result.columns.findIndex((c) => c.toLowerCase() === "id");
        const id = idIdx >= 0 ? Number(result.rows?.[0]?.[idIdx] ?? 0) : 0;
        // Refresh the local list entry in place so the caller sees the update
        // without a separate round-trip to re-list everything.
        const idx = currentItems.value.findIndex((it) => it.id === tc.id);
        if (idx >= 0) currentItems.value[idx] = { ...tc, id };
        return id > 0 ? id : null;
      }
      const sql = `INSERT INTO ${TC_FQN}
        (case_name, schema_name, object_name, object_type, package_name, pre_sql, call_sql, params_json, expected_return, note)
        VALUES (${qmarkIdx(1)},${qmarkIdx(2)},${qmarkIdx(3)},${qmarkIdx(4)},${qmarkIdx(5)},${qmarkIdx(6)},${qmarkIdx(7)},${qmarkIdx(8)},${qmarkIdx(9)},${qmarkIdx(10)})
        RETURNING id`;
      const result = await executeQuery(connectionId, database, sql, TC_SCHEMA, undefined, { maxRows: 1 });
      const idIdx = result.columns.findIndex((c) => c.toLowerCase() === "id");
      const id = idIdx >= 0 ? Number(result.rows?.[0]?.[idIdx] ?? 0) : 0;
      // Prepend the freshly saved row so the UI reflects it immediately.
      if (id > 0) currentItems.value.unshift({ ...tc, id });
      return id > 0 ? id : null;
    } catch (e: any) {
      error.value = e?.message ?? String(e);
      return null;
    }
  }

  async function deleteTestCase(connectionId: string, database: string, tc: PlsqlTestCase): Promise<boolean> {
    if (!tc.id) return false;
    try {
      const sql = `DELETE FROM ${TC_FQN} WHERE id=${qmarkIdx(1)}`;
      await executeQuery(connectionId, database, sql, TC_SCHEMA);
      currentItems.value = currentItems.value.filter((it) => it.id !== tc.id);
      return true;
    } catch (e: any) {
      error.value = e?.message ?? String(e);
      return false;
    }
  }

  async function runTestCase(connectionId: string, database: string, tc: PlsqlTestCase): Promise<any> {
    error.value = null;
    try {
      if (tc.pre_sql) {
        await executeQuery(connectionId, database, tc.pre_sql, tc.schema_name);
      }
      // Run in the routine's schema so package-qualified identifiers resolve.
      return await executeQuery(connectionId, database, tc.call_sql, tc.schema_name);
    } catch (e: any) {
      error.value = e?.message ?? String(e);
      return null;
    }
  }

  return {
    currentItems,
    currentGroupKey,
    loading,
    error,
    groupedItems,
    groupKey,
    listForObject,
    saveTestCase,
    deleteTestCase,
    runTestCase,
  };
});
