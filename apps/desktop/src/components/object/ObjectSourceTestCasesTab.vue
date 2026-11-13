<script setup lang="ts">
/**
 * 嵌入在对象源码编辑器旁的 "测试用例" tab。
 *
 * 树形结构：按 object_type 分一级节点（FUNCTION/PROCEDURE/PACKAGE 等），
 * 每个分组里是具体的用例（case_name + 最新更新时间）。右侧编辑区展示
 * 选中用例的完整定义，工具栏有 + 新建 / 保存 / 删除 / ▶ 执行。
 */
import { computed, onMounted, ref, watch } from "vue";
import { ChevronRight, ChevronDown, Plus, Save, Trash2, Play, RotateCcw } from "@lucide/vue";
import { usePlsqlTestCaseStore, type PlsqlTestCase, type PlsqlTestCaseParam } from "@/stores/plsqlTestCaseStore";
import { useConnectionStore } from "@/stores/connectionStore";
import { supportsPlsqlTestCases } from "@/lib/database/databaseObjectCapabilities";

const props = defineProps<{
  connectionId: string;
  database: string;
  schemaName: string;
  objectName: string;
  objectType: string;
  packageName?: string | null;
}>();

const store = usePlsqlTestCaseStore();
const connStore = useConnectionStore();
const expanded = ref<Set<string>>(new Set());
const selectedId = ref<number | null>(null);
const editing = ref<PlsqlTestCase | null>(null);
const dirty = ref(false);
const runResult = ref<any>(null);
const runError = ref<string | null>(null);

const supports = computed(() => {
  const c = connStore.getConfig(props.connectionId);
  return supportsPlsqlTestCases(c?.db_type);
});

async function refresh() {
  await store.listForObject(props.connectionId, props.database, props.schemaName, props.objectName, props.objectType, props.packageName);
  // Default: expand the first non-empty group.
  if (expanded.value.size === 0 && store.currentItems.length > 0) {
    const firstGroup = Object.keys(store.groupedItems())[0];
    if (firstGroup) expanded.value.add(firstGroup);
  }
}

onMounted(refresh);
watch(() => `${props.connectionId}/${props.database}/${props.schemaName}/${props.objectName}/${props.objectType}/${props.packageName ?? ""}`, refresh);

function groupedTree() {
  const groups = store.groupedItems();
  return Object.entries(groups).map(([kind, items]) => ({
    kind,
    items,
    expanded: expanded.value.has(kind),
  }));
}

function toggleGroup(kind: string) {
  if (expanded.value.has(kind)) expanded.value.delete(kind);
  else expanded.value.add(kind);
}

function select(tc: PlsqlTestCase) {
  selectedId.value = tc.id ?? null;
  editing.value = JSON.parse(JSON.stringify(tc));
  dirty.value = false;
  runResult.value = null;
  runError.value = null;
}

function newTestCase() {
  const skeleton = {
    case_name: `${props.objectName}_case_${Date.now().toString(36)}`,
    schema_name: props.schemaName,
    object_name: props.objectName,
    object_type: props.objectType,
    package_name: props.packageName ?? null,
    pre_sql: "",
    call_sql: `BEGIN\n  ${props.schemaName}.${props.objectName}();\nEND;`,
    params_json: JSON.stringify([]),
    expected_return: null,
    note: "",
  } as PlsqlTestCase;
  editing.value = skeleton;
  selectedId.value = null;
  dirty.value = true;
}

function markDirty() {
  dirty.value = true;
}

async function save() {
  if (!editing.value) return;
  const id = await store.saveTestCase(props.connectionId, props.database, editing.value);
  if (id != null) {
    editing.value.id = id;
    selectedId.value = id;
    dirty.value = false;
    await refresh();
  }
}

async function remove() {
  if (!editing.value || !editing.value.id) return;
  if (!confirm(`删除测试用例 "${editing.value.case_name}" ?`)) return;
  const ok = await store.deleteTestCase(props.connectionId, props.database, editing.value);
  if (ok) {
    editing.value = null;
    selectedId.value = null;
    dirty.value = false;
  }
}

async function run() {
  if (!editing.value) return;
  runResult.value = null;
  runError.value = null;
  const result = await store.runTestCase(props.connectionId, props.database, editing.value);
  if (store.error) runError.value = store.error;
  else runResult.value = result;
}

function parseParams(): PlsqlTestCaseParam[] {
  if (!editing.value) return [];
  try {
    const arr = JSON.parse(editing.value.params_json || "[]");
    if (Array.isArray(arr)) return arr;
  } catch {
    /* fall back */
  }
  return [];
}

function updateParams(arr: PlsqlTestCaseParam[]) {
  if (!editing.value) return;
  editing.value.params_json = JSON.stringify(arr);
  dirty.value = true;
}
</script>

<template>
  <div class="flex h-full w-full overflow-hidden text-sm">
    <!-- 左侧树形列表 -->
    <div class="flex w-72 shrink-0 flex-col border-r border-border/60 bg-muted/30">
      <div class="flex items-center gap-1 border-b border-border/60 p-2">
        <button class="inline-flex h-7 items-center gap-1 rounded px-2 text-xs hover:bg-background" :disabled="!supports" @click="newTestCase" title="新建测试用例"><Plus class="h-3.5 w-3.5" />新建</button>
        <button class="inline-flex h-7 items-center gap-1 rounded px-2 text-xs hover:bg-background" :disabled="!supports" @click="refresh" title="刷新"><RotateCcw class="h-3.5 w-3.5" />刷新</button>
        <span v-if="!supports" class="ml-auto text-xs text-muted-foreground">仅 Kingbase Oracle 模式可用</span>
      </div>

      <div v-if="store.loading" class="p-4 text-center text-xs text-muted-foreground">加载中…</div>
      <div v-else-if="store.error" class="p-4 text-xs text-destructive">{{ store.error }}</div>
      <div v-else-if="store.currentItems.length === 0" class="p-4 text-center text-xs text-muted-foreground">暂无测试用例 — 点"新建"或在调试结束后另存为测试用例</div>
      <ul v-else class="min-h-0 flex-1 overflow-auto py-1">
        <li v-for="g in groupedTree()" :key="g.kind">
          <button class="flex w-full items-center gap-1 px-2 py-1 text-left text-xs font-semibold hover:bg-background" @click="toggleGroup(g.kind)">
            <component :is="g.expanded ? ChevronDown : ChevronRight" class="h-3.5 w-3.5" />
            {{ g.kind }}
            <span class="ml-auto text-muted-foreground">{{ g.items.length }}</span>
          </button>
          <ul v-show="g.expanded">
            <li v-for="tc in g.items" :key="tc.id">
              <button class="flex w-full items-center gap-1 px-4 py-1 text-left text-xs hover:bg-background" :class="{ 'bg-primary/10 text-primary': selectedId === tc.id }" @click="select(tc)">
                <span class="truncate flex-1">{{ tc.case_name }}</span>
                <span v-if="tc.updated_at" class="shrink-0 text-[10px] text-muted-foreground">{{ tc.updated_at.slice(5, 16) }}</span>
              </button>
            </li>
          </ul>
        </li>
      </ul>
    </div>

    <!-- 右侧编辑区 -->
    <div class="flex min-w-0 flex-1 flex-col">
      <div v-if="!editing" class="flex h-full items-center justify-center text-sm text-muted-foreground">
        {{ store.currentItems.length > 0 ? "从左侧选择一个测试用例进行查看/编辑" : "点击「新建」创建第一个测试用例" }}
      </div>
      <template v-else>
        <div class="flex items-center gap-2 border-b border-border/60 px-3 py-2">
          <input class="h-7 w-48 rounded border border-border bg-background px-2 text-xs focus:outline-none focus:ring-2 focus:ring-primary/40" v-model="editing.case_name" @input="markDirty" placeholder="测试用例名称" />
          <select class="h-7 rounded border border-border bg-background px-2 text-xs focus:outline-none focus:ring-2 focus:ring-primary/40" v-model="editing.object_type" @change="markDirty">
            <option value="PROCEDURE">PROCEDURE</option>
            <option value="FUNCTION">FUNCTION</option>
            <option value="ANONYMOUS">ANONYMOUS BLOCK</option>
          </select>
          <div class="ml-auto flex items-center gap-1">
            <button class="inline-flex h-7 items-center gap-1 rounded px-2 text-xs hover:bg-muted" :disabled="!dirty" @click="save" title="保存"><Save class="h-3.5 w-3.5" />保存</button>
            <button v-if="editing.id" class="inline-flex h-7 items-center gap-1 rounded px-2 text-xs text-destructive hover:bg-muted" @click="remove" title="删除"><Trash2 class="h-3.5 w-3.5" />删除</button>
            <button class="inline-flex h-7 items-center gap-1 rounded bg-primary px-2 text-xs text-primary-foreground hover:bg-primary/90" @click="run" title="执行测试用例"><Play class="h-3.5 w-3.5" />执行</button>
          </div>
        </div>

        <div class="grid min-h-0 flex-1 grid-cols-2 gap-3 overflow-auto p-3">
          <!-- 左列：CALL SQL + 前置 SQL -->
          <div class="flex min-h-0 flex-col gap-2">
            <label class="text-xs font-medium text-muted-foreground">CALL / 匿名块</label>
            <textarea class="min-h-[180px] flex-1 resize-none rounded border border-border bg-background p-2 font-mono text-xs leading-relaxed focus:outline-none focus:ring-2 focus:ring-primary/40" v-model="editing.call_sql" @input="markDirty" spellcheck="false" />
            <label class="text-xs font-medium text-muted-foreground">前置 SQL（可选，执行前跑一次）</label>
            <textarea class="min-h-[80px] resize-none rounded border border-border bg-background p-2 font-mono text-xs leading-relaxed focus:outline-none focus:ring-2 focus:ring-primary/40" v-model="editing.pre_sql" @input="markDirty" spellcheck="false" />
          </div>

          <!-- 右列：参数表 + 备注 + 执行结果 -->
          <div class="flex min-h-0 flex-col gap-2">
            <label class="text-xs font-medium text-muted-foreground">参数（IN / OUT / INOUT）</label>
            <div class="overflow-auto rounded border border-border">
              <table class="w-full text-xs">
                <thead class="bg-muted/50">
                  <tr class="text-left text-muted-foreground">
                    <th class="px-2 py-1 font-medium">名称</th>
                    <th class="px-2 py-1 font-medium">方向</th>
                    <th class="px-2 py-1 font-medium">类型</th>
                    <th class="px-2 py-1 font-medium">初始值</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(p, idx) in parseParams()" :key="idx" class="border-t border-border/40">
                    <td class="px-2 py-1">
                      <input class="w-full bg-transparent focus:outline-none" v-model="p.name" @input="updateParams(parseParams())" />
                    </td>
                    <td class="px-2 py-1">
                      <select class="w-full bg-transparent focus:outline-none" v-model="p.direction" @change="updateParams(parseParams())">
                        <option value="IN">IN</option>
                        <option value="OUT">OUT</option>
                        <option value="INOUT">INOUT</option>
                      </select>
                    </td>
                    <td class="px-2 py-1">
                      <input class="w-full bg-transparent focus:outline-none" v-model="p.type" @input="updateParams(parseParams())" />
                    </td>
                    <td class="px-2 py-1">
                      <input class="w-full bg-transparent focus:outline-none" v-model="p.value" @input="updateParams(parseParams())" />
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
            <button
              class="h-7 w-fit rounded border border-border px-2 text-xs hover:bg-muted"
              @click="
                () => {
                  const arr = parseParams();
                  arr.push({ name: `p${arr.length + 1}`, direction: 'IN', type: 'TEXT', value: null });
                  updateParams(arr);
                }
              "
            >
              + 添加参数
            </button>

            <label class="mt-2 text-xs font-medium text-muted-foreground">备注</label>
            <textarea class="min-h-[60px] resize-none rounded border border-border bg-background p-2 text-xs focus:outline-none focus:ring-2 focus:ring-primary/40" v-model="editing.note" @input="markDirty" />

            <template v-if="runError || runResult">
              <label class="mt-2 text-xs font-medium text-muted-foreground">执行结果</label>
              <pre v-if="runError" class="overflow-auto rounded border border-destructive/40 bg-destructive/5 p-2 text-xs text-destructive">{{ runError }}</pre>
              <div v-else class="overflow-auto rounded border border-border bg-muted/20 p-2 text-xs">
                <div class="text-muted-foreground">Affected rows: {{ runResult?.affected_rows ?? 0 }}</div>
                <pre v-if="Array.isArray(runResult?.rows) && runResult.rows.length" class="mt-1 font-mono text-[11px]">{{ JSON.stringify(runResult.rows.slice(0, 20), null, 2) }}</pre>
              </div>
            </template>
          </div>
        </div>
      </template>
    </div>
  </div>
</template>
