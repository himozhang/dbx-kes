import { ref } from "vue";
import { defineStore } from "pinia";
import { dispatchAgentRpc } from "@/lib/backend/agentRpc";

export interface LogDirEntry {
  name: string;
  size: number;
  mode: string;
  mod_time: string;
  is_dir: boolean;
}

export interface LogLine {
  line_number: number;
  content: string;
}

export type LogBrowseMode = "local" | "ssh";

export interface SshParams {
  host: string;
  port: number;
  user: string;
  password?: string;
  private_key?: string;
  private_key_passphrase?: string;
  host_key_fingerprint?: string;
}

export const useLogBrowseStore = defineStore("logBrowse", () => {
  const mode = ref<LogBrowseMode>("local");
  const currentPath = ref<string>("");
  const entries = ref<LogDirEntry[]>([]);
  const lines = ref<LogLine[]>([]);
  const selectedFile = ref<string>("");
  const keyword = ref<string>("");
  const level = ref<string>("");
  const loading = ref(false);
  const error = ref<string | null>(null);
  const sshParams = ref<SshParams>({ host: "", port: 22, user: "" });

  async function configureRoots(connectionId: string, roots: string[]) {
    await dispatchAgentRpc(connectionId, "log_browse_configure_roots", { roots });
  }

  async function listDir(connectionId: string, path: string) {
    loading.value = true;
    error.value = null;
    try {
      const res = mode.value === "local" ? await dispatchAgentRpc(connectionId, "log_browse_list_dir", { path }) : await dispatchAgentRpc(connectionId, "log_browse_ssh_list_dir", { ...sshParams.value, path });
      entries.value = (res?.items ?? []) as LogDirEntry[];
      currentPath.value = path;
    } catch (e: any) {
      error.value = e?.message ?? String(e);
      entries.value = [];
    } finally {
      loading.value = false;
    }
  }

  async function readFile(connectionId: string, path: string, startLine = 1, limit = 500) {
    loading.value = true;
    error.value = null;
    try {
      const params = {
        path,
        start_line: startLine,
        limit,
        keyword: keyword.value,
        level: level.value,
      };
      const res = mode.value === "local" ? await dispatchAgentRpc(connectionId, "log_browse_read", params) : await dispatchAgentRpc(connectionId, "log_browse_ssh_read", { ...sshParams.value, ...params });
      lines.value = (res?.lines ?? []) as LogLine[];
      selectedFile.value = path;
    } catch (e: any) {
      error.value = e?.message ?? String(e);
      lines.value = [];
    } finally {
      loading.value = false;
    }
  }

  async function tailFile(connectionId: string, path: string, lastN = 200) {
    loading.value = true;
    error.value = null;
    try {
      const params = { path, last_n: lastN };
      const res = mode.value === "local" ? await dispatchAgentRpc(connectionId, "log_browse_tail", params) : await dispatchAgentRpc(connectionId, "log_browse_ssh_tail", { ...sshParams.value, ...params });
      lines.value = (res?.lines ?? []) as LogLine[];
      selectedFile.value = path;
    } catch (e: any) {
      error.value = e?.message ?? String(e);
      lines.value = [];
    } finally {
      loading.value = false;
    }
  }

  return {
    mode,
    currentPath,
    entries,
    lines,
    selectedFile,
    keyword,
    level,
    loading,
    error,
    sshParams,
    configureRoots,
    listDir,
    readFile,
    tailFile,
  };
});
