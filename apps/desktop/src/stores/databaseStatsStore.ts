import { ref } from "vue";
import { defineStore } from "pinia";
import { supportsPlsqlTestCases } from "@/lib/database/databaseObjectCapabilities";
import { executeQuery } from "@/lib/backend/tauri";
import type { DatabaseType } from "@/types/database";

export interface TopStatementRow {
  query: string;
  calls: number;
  total_exec_time: number;
  mean_exec_time: number;
  rows: number;
  shared_blks_hit?: number;
  shared_blks_read?: number;
}

export interface ActivityRow {
  pid: number;
  usename: string;
  application_name?: string;
  state?: string;
  query_start?: string;
  wait_event_type?: string;
  wait_event?: string;
  backend_type?: string;
  query?: string;
}

const STAT_TABLES = ["sys_stat_statements", "pg_stat_statements"];
const ACTIVITY_TABLES = ["sys_stat_activity", "pg_stat_activity"];

export const useDatabaseStatsStore = defineStore("databaseStats", () => {
  const topStatements = ref<TopStatementRow[]>([]);
  const activity = ref<ActivityRow[]>([]);
  const topLoading = ref(false);
  const activityLoading = ref(false);
  const error = ref<string | null>(null);

  async function refreshTop(connectionId: string, database: string, schema?: string) {
    topLoading.value = true;
    error.value = null;
    try {
      let rows: any[] = [];
      for (const table of STAT_TABLES) {
        const where = schema ? `WHERE query LIKE $1 ORDER BY mean_exec_time DESC LIMIT 50` : `ORDER BY mean_exec_time DESC LIMIT 50`;
        const args: any[] = schema ? [`${schema}.%`] : [];
        const res = await executeQuery(connectionId, database, `SELECT query, calls, total_exec_time, mean_exec_time, rows, shared_blks_hit, shared_blks_read FROM ${table} ${where}`, undefined, undefined, { maxRows: 50 });
        if (res?.rows && res.rows.length > 0) {
          rows = res.rows;
          break;
        }
        void args;
      }
      topStatements.value = rows as TopStatementRow[];
    } catch (e: any) {
      error.value = e?.message ?? String(e);
    } finally {
      topLoading.value = false;
    }
  }

  async function refreshActivity(connectionId: string, database: string) {
    activityLoading.value = true;
    error.value = null;
    try {
      let rows: any[] = [];
      for (const table of ACTIVITY_TABLES) {
        const res = await executeQuery(connectionId, database, `SELECT pid, usename, application_name, state, query_start, wait_event_type, wait_event, backend_type, query FROM ${table} WHERE datname = current_database() ORDER BY query_start ASC NULLS LAST`);
        if (res?.rows && res.rows.length > 0) {
          rows = res.rows;
          break;
        }
      }
      activity.value = rows as ActivityRow[];
    } catch (e: any) {
      error.value = e?.message ?? String(e);
    } finally {
      activityLoading.value = false;
    }
  }

  async function killPid(connectionId: string, database: string, pid: number) {
    try {
      // sys_terminate_backend for Kingbase V8; pg_terminate_backend as fallback.
      await executeQuery(connectionId, database, `SELECT sys_terminate_backend(${pid})`);
      await executeQuery(connectionId, database, `SELECT pg_terminate_backend(${pid})`);
      await refreshActivity(connectionId, database);
    } catch (e: any) {
      error.value = e?.message ?? String(e);
    }
  }

  function supports(databaseType?: DatabaseType, _compatibilityMode?: string): boolean {
    // Reuse the same guard as test cases — Kingbase is the gate.
    return supportsPlsqlTestCases(databaseType);
  }

  return {
    topStatements,
    activity,
    topLoading,
    activityLoading,
    error,
    refreshTop,
    refreshActivity,
    killPid,
    supports,
  };
});
