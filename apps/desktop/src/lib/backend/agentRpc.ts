/**
 * 通过 dbx-core → driver-agent → 目标 agent runtime 的 RPC 调度通路。
 *
 * dbx-core 的 `dispatch_agent_rpc` Tauri command 把任意 JSON-RPC 方法透传到
 * 当前连接已运行的 Agent runtime，供 log_browse_* / db_stats_* /
 * plsql_test_case_* / pl_debug_* 等无法用 SQL 表达的能力使用。
 *
 * 注意：Tauri v2 的命令参数按 camelCase 匹配 Rust 的 snake_case 形参，
 * 因此这里必须传 `connectionId`（而非 connection_id）。
 */

type AgentRpcParams = Record<string, unknown>;

export async function dispatchAgentRpc(connectionId: string, method: string, params: AgentRpcParams = {}): Promise<any> {
  const { invoke } = await import("@tauri-apps/api/core");
  return invoke("dispatch_agent_rpc", { connectionId, method, params });
}
