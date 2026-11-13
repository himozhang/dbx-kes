<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref } from "vue";
import { RefreshCw, Play } from "@lucide/vue";
import { useDatabaseStatsStore } from "@/stores/databaseStatsStore";

const props = defineProps<{ connectionId: string; database: string }>();

const store = useDatabaseStatsStore();

const active = ref<"top" | "activity">("top");
let timer: number | null = null;

onMounted(async () => {
  await store.refreshTop(props.connectionId, props.database);
  await store.refreshActivity(props.connectionId, props.database);
  timer = window.setInterval(() => {
    void store.refreshActivity(props.connectionId, props.database);
  }, 5000);
});

onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer);
});

async function refresh() {
  await store.refreshTop(props.connectionId, props.database);
  await store.refreshActivity(props.connectionId, props.database);
}

async function kill(pid: number) {
  if (!confirm(`终止会话 PID=${pid} ?`)) return;
  await store.killPid(props.connectionId, props.database, pid);
}

function levelOf(execTime: number): string {
  if (execTime > 1000) return "text-destructive";
  if (execTime > 100) return "text-amber-500";
  return "text-muted-foreground";
}
</script>

<template>
  <div class="flex h-full w-full flex-col text-sm">
    <!-- 头部工具栏 -->
    <div class="flex items-center gap-2 border-b border-border/60 bg-muted/20 px-3 py-2">
      <button class="h-7 rounded px-2 text-xs hover:bg-background" :class="active === 'top' ? 'bg-primary text-primary-foreground' : ''" @click="active = 'top'">Top SQL</button>
      <button class="h-7 rounded px-2 text-xs hover:bg-background" :class="active === 'activity' ? 'bg-primary text-primary-foreground' : ''" @click="active = 'activity'">活动会话</button>
      <button class="ml-auto inline-flex h-7 items-center gap-1 rounded px-2 text-xs hover:bg-background" @click="refresh"><RefreshCw class="h-3.5 w-3.5" />刷新</button>
    </div>

    <!-- 错误 / 未安装 stat_statements 提示 -->
    <div v-if="store.error" class="border-b border-border/60 bg-destructive/5 px-3 py-2 text-xs text-destructive">
      {{ store.error }}
    </div>

    <!-- Top SQL -->
    <div v-show="active === 'top'" class="min-h-0 flex-1 overflow-auto">
      <div v-if="store.topLoading" class="p-8 text-center text-xs text-muted-foreground">加载中…</div>
      <div v-else-if="store.topStatements.length === 0" class="p-8 text-center text-xs text-muted-foreground">stat_statements 扩展无数据，请执行 <code class="rounded bg-muted px-1">CREATE EXTENSION sys_stat_statements</code> 后重启 KingbaseES</div>
      <table v-else class="w-full text-xs">
        <thead class="sticky top-0 bg-muted/80 text-left text-muted-foreground backdrop-blur">
          <tr>
            <th class="px-3 py-1.5 font-medium">调用次数</th>
            <th class="px-3 py-1.5 font-medium">总耗时 (ms)</th>
            <th class="px-3 py-1.5 font-medium">平均 (ms)</th>
            <th class="px-3 py-1.5 font-medium">行数</th>
            <th class="px-3 py-1.5 font-medium">SQL</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, i) in store.topStatements" :key="i" class="border-t border-border/40 hover:bg-muted/30">
            <td class="px-3 py-1">{{ row.calls }}</td>
            <td class="px-3 py-1">{{ Number(row.total_exec_time).toFixed(1) }}</td>
            <td class="px-3 py-1" :class="levelOf(row.mean_exec_time)">{{ Number(row.mean_exec_time).toFixed(2) }}</td>
            <td class="px-3 py-1">{{ row.rows }}</td>
            <td class="px-3 py-1 font-mono text-[11px] text-muted-foreground">{{ row.query?.slice(0, 200) }}<span v-if="row.query && row.query.length > 200">…</span></td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 活动会话 -->
    <div v-show="active === 'activity'" class="min-h-0 flex-1 overflow-auto">
      <div v-if="store.activityLoading" class="p-8 text-center text-xs text-muted-foreground">加载中…</div>
      <div v-else-if="store.activity.length === 0" class="p-8 text-center text-xs text-muted-foreground">当前无活动会话</div>
      <table v-else class="w-full text-xs">
        <thead class="sticky top-0 bg-muted/80 text-left text-muted-foreground backdrop-blur">
          <tr>
            <th class="px-3 py-1.5 font-medium">PID</th>
            <th class="px-3 py-1.5 font-medium">用户</th>
            <th class="px-3 py-1.5 font-medium">应用</th>
            <th class="px-3 py-1.5 font-medium">状态</th>
            <th class="px-3 py-1.5 font-medium">启动时间</th>
            <th class="px-3 py-1.5 font-medium">执行 SQL</th>
            <th class="px-3 py-1.5"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, i) in store.activity" :key="i" class="border-t border-border/40 hover:bg-muted/30">
            <td class="px-3 py-1 font-mono">{{ row.pid }}</td>
            <td class="px-3 py-1">{{ row.usename }}</td>
            <td class="px-3 py-1 truncate max-w-[120px]">{{ row.application_name ?? "-" }}</td>
            <td class="px-3 py-1">
              <span class="rounded px-1.5 py-0.5 text-[11px]" :class="row.state === 'active' ? 'bg-amber-500/20 text-amber-600' : row.state ? 'bg-muted text-muted-foreground' : ''">{{ row.state ?? "idle" }}</span>
            </td>
            <td class="px-3 py-1 text-muted-foreground">{{ row.query_start?.slice(5, 19) ?? "-" }}</td>
            <td class="px-3 py-1 font-mono text-[11px] text-muted-foreground max-w-[400px] truncate" :title="row.query ?? ''">{{ row.query?.slice(0, 160) ?? "-" }}</td>
            <td class="px-3 py-1">
              <button class="inline-flex h-6 items-center gap-0.5 rounded bg-destructive/10 px-1.5 text-[11px] text-destructive hover:bg-destructive/20" @click="kill(Number(row.pid))"><Play class="h-3 w-3" style="transform: rotate(180deg)" />终止</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
