<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref, watch } from "vue";
import { RefreshCw, Play, Pause, Folder, FileText, ChevronRight } from "@lucide/vue";
import { useLogBrowseStore } from "@/stores/logBrowseStore";
import { dispatchAgentRpc } from "@/lib/backend/agentRpc";

const props = defineProps<{ connectionId: string; database: string }>();

const store = useLogBrowseStore();

const tailMode = ref(false);
let tailTimer: number | null = null;

const pathSegments = computed(() => {
  if (!store.currentPath) return [];
  return store.currentPath.split("/").filter(Boolean);
});

const logLevels = ["", "ERROR", "WARN", "INFO", "DEBUG", "TRACE"];

function lineClass(content: string): string {
  const upper = content.toUpperCase();
  if (/\b(FATAL|PANIC|ERROR)\b/.test(upper)) return "text-red-500";
  if (/\b(WARN|WARNING)\b/.test(upper)) return "text-amber-500";
  if (/\b(NOTICE|INFO)\b/.test(upper)) return "text-foreground";
  if (/\bDEBUG\b/.test(upper)) return "text-blue-500";
  if (/\bTRACE\b/.test(upper)) return "text-purple-500";
  return "text-muted-foreground";
}

async function loadDir(path: string) {
  await store.listDir(props.connectionId, path);
}

async function openFile(path: string) {
  store.selectedFile = path;
  if (tailMode.value) {
    await store.tailFile(props.connectionId, path);
  } else {
    await store.readFile(props.connectionId, path);
  }
}

async function refresh() {
  if (store.selectedFile) {
    await openFile(store.selectedFile);
  } else if (store.currentPath) {
    await loadDir(store.currentPath);
  }
}

async function navigateToSegment(index: number) {
  const segments = pathSegments.value.slice(0, index + 1);
  const path = "/" + segments.join("/");
  await loadDir(path);
}

function toggleTail() {
  tailMode.value = !tailMode.value;
  if (tailMode.value) {
    startTail();
  } else {
    stopTail();
  }
}

function startTail() {
  stopTail();
  if (!store.selectedFile) return;
  tailTimer = window.setInterval(() => {
    void store.tailFile(props.connectionId, store.selectedFile);
  }, 2000);
}

function stopTail() {
  if (tailTimer) {
    window.clearInterval(tailTimer);
    tailTimer = null;
  }
}

async function connectSsh() {
  // Trigger a directory listing with the SSH params to validate the connection.
  await store.listDir(props.connectionId, "/");
}

// Initialize: probe the server's data directory to set a sensible default log path.
onMounted(async () => {
  try {
    const res = await dispatchAgentRpc(props.connectionId, "db_stats_data_directory", {});
    const dataDir = res?.directory as string | undefined;
    if (dataDir) {
      const logDir = dataDir.replace(/\/$/, "") + "/sys_log";
      await store.configureRoots(props.connectionId, [logDir]);
      await loadDir(logDir);
    } else {
      await loadDir("/");
    }
  } catch {
    // Fall back to root listing; user can navigate or configure roots manually.
    await loadDir("/");
  }
});

onBeforeUnmount(stopTail);

watch(
  () => store.keyword,
  () => {
    if (store.selectedFile && !tailMode.value) void store.readFile(props.connectionId, store.selectedFile);
  },
);
watch(
  () => store.level,
  () => {
    if (store.selectedFile && !tailMode.value) void store.readFile(props.connectionId, store.selectedFile);
  },
);
watch(tailMode, (on) => {
  if (on) startTail();
  else stopTail();
});
</script>

<template>
  <div class="flex h-full w-full flex-col text-sm">
    <!-- 顶部工具栏：模式切换 + 过滤 -->
    <div class="flex flex-wrap items-center gap-2 border-b border-border/60 bg-muted/20 px-3 py-2">
      <div class="flex rounded border border-border">
        <button class="h-7 px-2 text-xs" :class="store.mode === 'local' ? 'bg-primary text-primary-foreground' : 'hover:bg-background'" @click="store.mode = 'local'">本地</button>
        <button class="h-7 px-2 text-xs" :class="store.mode === 'ssh' ? 'bg-primary text-primary-foreground' : 'hover:bg-background'" @click="store.mode = 'ssh'">远端 SSH</button>
      </div>

      <button class="inline-flex h-7 items-center gap-1 rounded px-2 text-xs hover:bg-background" @click="refresh"><RefreshCw class="h-3.5 w-3.5" />刷新</button>

      <div class="ml-auto flex items-center gap-2">
        <input v-model="store.keyword" placeholder="关键字过滤" class="h-7 w-36 rounded border border-border bg-background px-2 text-xs outline-none focus:ring-1 focus:ring-primary" />
        <select v-model="store.level" class="h-7 rounded border border-border bg-background px-1 text-xs">
          <option v-for="lv in logLevels" :key="lv" :value="lv">{{ lv || "全部级别" }}</option>
        </select>
        <button class="inline-flex h-7 items-center gap-1 rounded px-2 text-xs" :class="tailMode ? 'bg-red-500/10 text-red-500' : 'hover:bg-background'" @click="toggleTail"><component :is="tailMode ? Pause : Play" class="h-3.5 w-3.5" />{{ tailMode ? "停止" : "实时" }}</button>
      </div>
    </div>

    <!-- SSH 参数栏 -->
    <div v-if="store.mode === 'ssh'" class="flex flex-wrap items-center gap-2 border-b border-border/60 bg-muted/10 px-3 py-2 text-xs">
      <input v-model="store.sshParams.host" placeholder="主机" class="h-7 w-32 rounded border border-border bg-background px-2 outline-none" />
      <input v-model.number="store.sshParams.port" type="number" placeholder="端口" class="h-7 w-16 rounded border border-border bg-background px-2 outline-none" />
      <input v-model="store.sshParams.user" placeholder="用户" class="h-7 w-24 rounded border border-border bg-background px-2 outline-none" />
      <input v-model="store.sshParams.password" type="password" placeholder="密码" class="h-7 w-28 rounded border border-border bg-background px-2 outline-none" />
      <input v-model="store.sshParams.host_key_fingerprint" placeholder="主机指纹 (SHA256:..., 可空)" class="h-7 w-56 rounded border border-border bg-background px-2 outline-none" />
      <button class="h-7 rounded bg-primary px-2 text-xs text-primary-foreground hover:bg-primary/90" @click="connectSsh">连接</button>
    </div>

    <!-- 错误提示 -->
    <div v-if="store.error" class="border-b border-border/60 bg-destructive/5 px-3 py-2 text-xs text-destructive">
      {{ store.error }}
    </div>

    <!-- 路径面包屑 -->
    <div v-if="store.currentPath" class="flex items-center gap-1 border-b border-border/40 px-3 py-1.5 text-xs text-muted-foreground">
      <button class="hover:text-foreground" @click="loadDir('/')">/</button>
      <template v-for="(seg, i) in pathSegments" :key="i">
        <ChevronRight class="h-3 w-3" />
        <button class="hover:text-foreground" @click="navigateToSegment(i)">{{ seg }}</button>
      </template>
    </div>

    <div class="flex min-h-0 flex-1">
      <!-- 左侧目录树 -->
      <div class="w-64 shrink-0 overflow-auto border-r border-border/60">
        <div v-if="store.loading && store.entries.length === 0" class="p-4 text-center text-xs text-muted-foreground">加载中…</div>
        <div v-else-if="store.entries.length === 0" class="p-4 text-center text-xs text-muted-foreground">空目录</div>
        <ul v-else class="py-1">
          <li
            v-for="entry in store.entries"
            :key="entry.name"
            class="flex cursor-pointer items-center gap-1.5 px-2 py-1 hover:bg-muted/40"
            :class="store.selectedFile.endsWith(entry.name) ? 'bg-muted/60' : ''"
            @click="entry.is_dir ? loadDir((store.currentPath.endsWith('/') ? store.currentPath : store.currentPath + '/') + entry.name) : openFile((store.currentPath.endsWith('/') ? store.currentPath : store.currentPath + '/') + entry.name)"
          >
            <component :is="entry.is_dir ? Folder : FileText" class="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
            <span class="truncate">{{ entry.name }}</span>
            <span v-if="!entry.is_dir" class="ml-auto text-[10px] text-muted-foreground">{{ entry.size }}</span>
          </li>
        </ul>
      </div>

      <!-- 右侧日志内容 -->
      <div class="min-w-0 flex-1 overflow-auto bg-background font-mono text-xs">
        <div v-if="!store.selectedFile" class="p-8 text-center text-muted-foreground">选择左侧日志文件查看内容</div>
        <div v-else-if="store.loading && store.lines.length === 0" class="p-8 text-center text-muted-foreground">加载中…</div>
        <div v-else-if="store.lines.length === 0" class="p-8 text-center text-muted-foreground">无匹配日志行</div>
        <div v-else class="whitespace-pre px-3 py-2 leading-relaxed">
          <div v-for="line in store.lines" :key="line.line_number" class="flex">
            <span class="mr-3 w-16 shrink-0 select-none text-right text-muted-foreground/50">{{ line.line_number }}</span>
            <span :class="lineClass(line.content)">{{ line.content }}</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
