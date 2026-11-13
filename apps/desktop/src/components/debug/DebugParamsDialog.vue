<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { Loader2 } from "@lucide/vue";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { usePlDebugStore } from "@/stores/plDebugStore";
import type { PlDebugParam, PlDebugStartRequest } from "@/lib/backend/pl-debug-tauri";
import type { PlDebugArgument, PlDebugInspectResult, PlDebugSourceField } from "@/lib/debug/plDebugProtocol";
import { canonicalPlDebugScalarType, compositeArgumentShape, compositeInputComplete, compositeNode, emptyCompositeValue, type CompositeRoot, type CompositeValue } from "@/lib/debug/plDebugComposite";
import PlDebugCompositeEditor from "./PlDebugCompositeEditor.vue";
import PlDebugJsonEditor from "./PlDebugJsonEditor.vue";

const { t } = useI18n();
const store = usePlDebugStore();

const open = defineModel<boolean>("open", { default: false });

const props = defineProps<{
  /** PROCEDURE | FUNCTION | ANONYMOUS */
  objectType: string;
  objectName?: string;
  packageName?: string;
  source?: string;
  schema?: string;
  preview: PlDebugInspectResult | null;
  /** True while the caller is still resolving `params` from database metadata. */
  loading?: boolean;
  /** Metadata lookup failure, reported instead of the empty-parameter hint. */
  loadError?: string | null;
  database: string;
  connectionId: string;
  allowTypePreparation?: boolean;
}>();

const emit = defineEmits<{ started: [debugId: string]; recover: []; prepareTypes: [] }>();

// 调试入口已经锁定了具体对象，直接使用第一个（也是唯一）候选签名，
// 不再让用户在重载列表里选择。
const drafts = ref<PlDebugParam[][]>([]);
const candidates = computed(() => props.preview?.targetPreview?.candidates ?? []);
const selected = computed(() => candidates.value[0]);
const editableParams = computed(() => drafts.value[0] ?? []);
const anonymous = computed(() => props.objectType === "ANONYMOUS");
/** 只读签名行：告诉用户当前调试的是哪个对象，不提供重载切换。 */
const signatureLabel = computed(() => {
  const candidate = selected.value;
  if (!candidate) return "";
  const signature = candidate.signature;
  const owner = signature.packageName ? `${signature.packageName}.` : "";
  return `${signature.schema}.${owner}${signature.objectName}(${candidate.parameters.map((param) => `${param.name} ${param.mode} ${argumentType(signature, param)}`).join(", ")})`;
});
const starting = ref(false);
const errorMessage = ref<string | null>(null);
let previousCandidates: typeof candidates.value = [];
const signatureKey = (candidate: (typeof candidates.value)[number]) => {
  const signature = candidate.signature;
  return JSON.stringify([signature.schema, signature.packageName, signature.objectName, signature.objectId, signature.subprogramId, signature.objectType, signature.arguments]);
};

/** json / jsonb 参数使用 JSON 编辑器而不是普通输入框。 */
function isJsonParamType(type: string) {
  return /^(jsonb?|jsonb?\s*\(.*\))$/i.test((type ?? "").trim()) || /\.(jsonb?)$/i.test((type ?? "").trim());
}

/** 源码里该参数实际引用到的 JSON 字段。 */
function sourceFields(paramName: string): PlDebugSourceField[] {
  const fields = selected.value?.sourceAnalysis?.jsonFields ?? [];
  const key = paramName.toLowerCase();
  return fields.filter((field) => field.parameter.toLowerCase() === key);
}

/**
 * 按字段名生成贴近真实业务的测试值：主键/数量给数字，布尔标记给 true，
 * 时间给可解析的时间戳字符串，其余给稳定的示例文本。
 */
function sampleJSONValue(path: string): unknown {
  const key = (path.split(".").pop() ?? "").toLowerCase();
  if (/^(is_|has_|can_|allow_|enable_|disable_)/.test(key) || /_(flag|enabled|deleted|valid)$/.test(key)) return true;
  if (key === "id" || /_(id|count|qty|quantity|num|number|amount|price|total|age|score|seq|version)$/.test(key)) return 1;
  if (/(_at|_time|_date)$/.test(key) || key === "date" || key === "time") return "2026-01-01 00:00:00";
  if (key.includes("email")) return "test@example.com";
  if (key.endsWith("name")) return "test";
  return "test";
}

/** 只构建源码真正读取的键，未引用的字段不会出现。 */
function buildJSONFromSource(paramName: string): string {
  const fields = sourceFields(paramName);
  const root: Record<string, unknown> = {};
  for (const field of fields) {
    const parts = field.path.split(".").filter(Boolean);
    let cursor: Record<string, unknown> = root;
    parts.forEach((part, index) => {
      if (index === parts.length - 1) {
        cursor[part] = sampleJSONValue(field.path);
        return;
      }
      if (typeof cursor[part] !== "object" || cursor[part] === null) cursor[part] = {};
      cursor = cursor[part] as Record<string, unknown>;
    });
  }
  return JSON.stringify(root, null, 2);
}

const jsonHint = (param: PlDebugParam) => {
  const analysis = selected.value?.sourceAnalysis;
  if (!analysis?.available) return analysis?.message || t("plDebug.start.jsonSourceUnavailable");
  const count = sourceFields(param.name).length;
  return count > 0 ? t("plDebug.start.jsonGeneratedFrom", { count }) : t("plDebug.start.jsonNoSourceFields");
};

function regenerateJSON(param: PlDebugParam) {
  param.value = buildJSONFromSource(param.name);
}

const NUMERIC_PARAM_TYPES = new Set(["SMALLINT", "INT", "INT2", "INT4", "INT8", "INTEGER", "BIGINT", "NUMBER", "NUMERIC", "DECIMAL", "DEC", "REAL", "FLOAT", "FLOAT4", "FLOAT8", "DOUBLE", "DOUBLE PRECISION", "MONEY", "SERIAL", "SMALLSERIAL", "BIGSERIAL"]);

/** 本地日期（避免 UTC 偏移把“当天”变成前一天）。 */
function localDateText() {
  const now = new Date();
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

/** 按数据类型给出可直接执行的默认入参，避免用户面对空白输入框。 */
function defaultParamValue(type: string): string {
  const normalized = canonicalPlDebugScalarType(
    (type ?? "")
      .trim()
      .toUpperCase()
      .replace(/\([^)]*\)/g, "")
      .replace(/\s+/g, " ")
      .trim(),
  );
  if (normalized === "PL/SQL BOOLEAN" || normalized === "BOOLEAN" || normalized === "BOOL") return "false";
  // 时区类型使用结构化编辑器，默认值在 parameterDraft 里单独填充。
  if (normalized.includes("TIME ZONE")) return "";
  if (normalized === "DATE") return `${localDateText()}T00:00:00`;
  if (normalized === "DATETIME" || normalized === "TIMESTAMP" || normalized === "TIMESTAMP WITHOUT TIME ZONE") return `${localDateText()}T00:00:00`;
  if (normalized === "TIME" || normalized === "TIME WITHOUT TIME ZONE") return "00:00:00";
  // 这两个类型是 IEEE-754 十六进制文本，给 1.0 的位模式。
  if (normalized === "BINARY_FLOAT") return "3F800000";
  if (normalized === "BINARY_DOUBLE") return "3FF0000000000000";
  if (NUMERIC_PARAM_TYPES.has(normalized)) return "0";
  return "";
}

function parameterDraft(candidate: (typeof candidates.value)[number], param: PlDebugArgument): PlDebugParam {
  const draft: PlDebugParam = { name: param.name ?? "", mode: param.mode, type: param.type, value: defaultParamValue(param.type), ...(param.mode === "OUT" ? {} : { inputState: param.defaulted === "Y" ? "default" : "value" }) };
  if (param.mode !== "OUT") {
    // json/jsonb 直接展开为 JSON 编辑器，值由源码引用清单自动生成。
    if (isJsonParamType(param.type)) draft.value = buildJSONFromSource(param.name ?? "");
    const shape = compositeArgumentShape(candidate.signature, param);
    if (shape && compositeNode(candidate.signature, shape)) {
      draft.value = null;
      draft.typedValue = emptyCompositeValue(candidate.signature, shape) as CompositeRoot;
    }
    if (canonicalPlDebugScalarType(param.type) === "TIMESTAMP WITH TIME ZONE") draft.typedValue = { kind: "timestamp_tz", value: { local: `${localDateText()}T00:00:00`, offset: "+00:00", region: null } };
    if (canonicalPlDebugScalarType(param.type) === "TIMESTAMP WITH LOCAL TIME ZONE") draft.typedValue = { kind: "timestamp_ltz", value: { instant: `${localDateText()}T00:00:00Z` } };
  }
  return draft;
}

const shapeFor = (index: number) => compositeArgumentShape(selected.value?.signature, selected.value?.parameters[index]);
function argumentType(signature: (typeof candidates.value)[number]["signature"], argument: PlDebugArgument) {
  const reference = compositeArgumentShape(signature, argument)?.reference;
  return reference ? [reference.owner, reference.packageName, reference.typeName].filter(Boolean).join(".") : argument.type;
}
function compositeComplete(param: PlDebugParam, index: number) {
  if (param.mode !== "OUT" && param.inputState === "value") {
    if (isJsonParamType(param.type)) {
      try {
        JSON.parse(param.value ?? "");
        return true;
      } catch {
        return false;
      }
    }
    if (param.type === "BINARY_FLOAT") return /^[0-9a-fA-F]{8}$/.test(param.value ?? "");
    if (param.type === "BINARY_DOUBLE") return /^[0-9a-fA-F]{16}$/.test(param.value ?? "");
  }
  const shape = shapeFor(index);
  return (
    !shape ||
    param.mode === "OUT" ||
    param.inputState === "default" ||
    (!!selected.value && !!param.typedValue && (param.typedValue.kind === "record" || param.typedValue.kind === "collection" || param.typedValue.kind === "object") && compositeInputComplete(selected.value.signature, shape, param.typedValue))
  );
}
function updateCompositeParam(param: PlDebugParam, value: CompositeValue) {
  if (value.kind === "record" || value.kind === "collection" || value.kind === "object") param.typedValue = value;
}

function updateTimezoneParam(param: PlDebugParam, field: "local" | "offset" | "region" | "instant", value: string | number) {
  const typed = param.typedValue;
  if (typed?.kind === "timestamp_tz" && field !== "instant") {
    if (field === "region") typed.value.region = String(value) || null;
    else typed.value[field] = String(value);
  }
  if (typed?.kind === "timestamp_ltz" && field === "instant") typed.value.instant = String(value);
}

watch(
  () => props.preview,
  () => {
    errorMessage.value = null;
    if (!props.preview) return;
    const previousDrafts = new Map(previousCandidates.map((candidate, index) => [signatureKey(candidate), drafts.value[index]]));
    drafts.value = candidates.value.map((candidate) => {
      const previous = previousDrafts.get(signatureKey(candidate));
      return candidate.parameters.map((argument, index) => {
        const initial = parameterDraft(candidate, argument);
        const saved = previous?.[index];
        if (!saved) return initial;
        const draft = JSON.parse(JSON.stringify(saved)) as PlDebugParam;
        if (candidate.signature.signatureVersion === 2 && !draft.typedValue && (initial.typedValue?.kind === "record" || initial.typedValue?.kind === "collection" || initial.typedValue?.kind === "object")) return { ...draft, value: null, typedValue: initial.typedValue };
        return draft;
      });
    });
    previousCandidates = candidates.value;
  },
  { immediate: true },
);

const title = computed(() => {
  const target = anonymous.value ? t("plDebug.launch.anonymousTitle") : props.packageName ? `${props.packageName}.${props.objectName}` : (props.objectName ?? "");
  return `${t("plDebug.start.title")} — ${target}`;
});

const startRequest = computed<PlDebugStartRequest>(() => ({
  schema: props.schema ?? null,
  objectType: props.objectType,
  objectName: props.objectName ?? null,
  packageName: props.packageName ?? null,
  source: props.source ?? null,
  ...(anonymous.value
    ? {}
    : {
        targetSignature: selected.value?.signature,
        ...(selected.value?.signature.signatureVersion === 2 ? { metadataPreparation: "type_only" as const } : {}),
        params: editableParams.value.map((param) => {
          if (param.mode === "OUT" || param.inputState !== "value") {
            const value = { ...param, value: null };
            delete value.typedValue;
            return value;
          }
          if (selected.value?.signature.signatureVersion === 2) {
            if (param.typedValue?.kind === "record" || param.typedValue?.kind === "collection" || param.typedValue?.kind === "object") return { ...param, value: null };
            if (param.type === "PL/SQL BOOLEAN") return { ...param, value: null, typedValue: { kind: "boolean" as const, value: param.value === "true" } };
            if (param.type === "DATE") return { ...param, value: null, typedValue: { kind: "date" as const, value: param.value ?? "" } };
            if (param.type === "TIMESTAMP") return { ...param, value: null, typedValue: { kind: "timestamp" as const, value: param.value ?? "" } };
            if (param.type === "CLOB") return { ...param, value: null, typedValue: { kind: "clob" as const, value: param.value ?? "" } };
            if (param.type === "NCLOB") return { ...param, value: null, typedValue: { kind: "nclob" as const, value: param.value ?? "" } };
            if (param.type === "BINARY_FLOAT") return { ...param, value: null, typedValue: { kind: "binary_float" as const, value: param.value ?? "" } };
            if (param.type === "BINARY_DOUBLE") return { ...param, value: null, typedValue: { kind: "binary_double" as const, value: param.value ?? "" } };
            if (param.type === "NCHAR" || param.type === "NVARCHAR2" || param.type === "INTERVAL YEAR TO MONTH" || param.type === "INTERVAL DAY TO SECOND") return { ...param, value: null, typedValue: { kind: "text" as const, value: param.value ?? "" } };
            if (param.type === "BLOB") return { ...param, value: null, typedValue: { kind: "blob" as const, value: { encoding: "base64" as const, data: param.value ?? "" } } };
            if (param.type === "RAW") return { ...param, value: null, typedValue: { kind: "raw" as const, value: { encoding: "base64" as const, data: param.value ?? "" } } };
            if (canonicalPlDebugScalarType(param.type) === "TIMESTAMP WITH TIME ZONE" || canonicalPlDebugScalarType(param.type) === "TIMESTAMP WITH LOCAL TIME ZONE") return { ...param, value: null };
          }
          return { ...param };
        }),
      }),
}));
const previewError = computed(() => {
  if (props.loadError) return props.loadError;
  if (!props.preview || props.loading) return null;
  if (props.preview.capabilities.supported !== true) return String(props.preview.capabilities.reason || t("plDebug.launch.unsupported"));
  if (anonymous.value) return null;
  if (candidates.value.length === 0) return props.preview.targetPreview?.message || t("plDebug.start.metadataUnavailable");
  if (selected.value?.signature.status !== undefined && selected.value.signature.status !== "VALID") return t("plDebug.launch.invalidObject");
  if (selected.value?.parameterTransport === "not_implemented") return selected.value.message || t("plDebug.start.metadataUnavailable");
  return null;
});
const analysisFields = computed(() => selected.value?.sourceAnalysis?.jsonFields ?? []);
function sqlLiteral(param: PlDebugParam): string {
  if (param.mode === "OUT" || param.inputState === "null") return "NULL";
  if (isJsonParamType(param.type) && param.inputState === "value" && !(param.value ?? "").trim()) return "NULL";
  if (param.type === "PL/SQL BOOLEAN") return param.value === "true" ? "TRUE" : "FALSE";
  if (param.typedValue?.kind === "boolean") return param.typedValue.value ? "TRUE" : "FALSE";
  const value = (param.value ?? "").trim();
  if (!value) return "NULL";
  if (/^(number|int|serial|numeric|decimal|real|float|double|money)/i.test(param.type.trim()) && /^[-+]?\d+(\.\d+)?([eE][-+]?\d+)?$/.test(value)) return value;
  return `'${value.replace(/'/g, "''")}'`;
}
/** 底部预览：与 agent 最终执行的目标语句保持一致（CALL / SELECT ... FROM DUAL）。 */
const generatedCode = computed(() => {
  if (anonymous.value) return (props.source ?? "").trim();
  if (!selected.value) return "";
  const signature = selected.value.signature;
  const qualified = [signature.schema, signature.packageName, signature.objectName].filter(Boolean).join(".");
  const call = `${qualified}(${editableParams.value.map(sqlLiteral).join(", ")})`;
  return signature.objectType === "FUNCTION" ? `SELECT ${call} FROM DUAL;` : `CALL ${call};`;
});
const canStart = computed(
  () =>
    !starting.value &&
    !store.pendingStart &&
    !store.recoveryRequired &&
    !store.compilationBusy &&
    !(store.compilationRef?.connectionId === props.connectionId && store.compilationUnresolved) &&
    !props.loading &&
    !previewError.value &&
    !!props.preview?.connectionRevision &&
    props.preview.capabilities.supported === true &&
    (anonymous.value || (!!selected.value && editableParams.value.every(compositeComplete))),
);

async function recoverDebug() {
  if (starting.value) return;
  starting.value = true;
  errorMessage.value = null;
  const connectionId = props.connectionId;
  const database = props.database;
  const fingerprint = JSON.stringify(startRequest.value);
  try {
    const result = await store.recover();
    if (result && open.value && props.connectionId === connectionId && props.database === database && JSON.stringify(startRequest.value) === fingerprint) {
      open.value = false;
      emit("recover");
    }
  } catch (caught) {
    errorMessage.value = String(caught);
  } finally {
    starting.value = false;
  }
}

async function startDebug() {
  if (!canStart.value) return;
  starting.value = true;
  errorMessage.value = null;
  try {
    const request = JSON.parse(JSON.stringify(startRequest.value)) as PlDebugStartRequest;
    const connectionId = props.connectionId;
    const database = props.database;
    const revision = props.preview!.connectionRevision;
    const fingerprint = JSON.stringify(request);
    const isCurrent = () => open.value && props.connectionId === connectionId && props.database === database && props.preview?.connectionRevision === revision && JSON.stringify(startRequest.value) === fingerprint;
    const result = await store.start(connectionId, database, request, { riskAccepted: true, expectedConnectionRevision: revision, isCurrent });
    if (isCurrent()) open.value = false;
    emit("started", result.debugId ?? "");
  } catch (caught) {
    errorMessage.value = String(caught);
  } finally {
    starting.value = false;
  }
}
</script>

<template>
  <Dialog v-model:open="open">
    <DialogContent class="max-w-2xl overflow-y-auto">
      <DialogHeader>
        <DialogTitle>{{ title }}</DialogTitle>
      </DialogHeader>
      <div class="flex items-center gap-2">
        <p v-if="!anonymous" class="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground" :title="signatureLabel">{{ signatureLabel }}</p>
        <Button v-if="allowTypePreparation && selected?.signature.signatureVersion !== 2" variant="outline" size="sm" :disabled="starting || loading" @click="emit('prepareTypes')">{{ t("plDebug.start.prepareTypes") }}</Button>
      </div>
      <pre v-if="anonymous" class="max-h-40 overflow-auto whitespace-pre-wrap break-all rounded-md border bg-muted/30 p-2 font-mono text-xs">{{ source }}</pre>
      <p v-if="selected?.returnValue" class="text-sm">
        {{ t("plDebug.start.returnType") }}: <span class="break-all font-mono">{{ argumentType(selected.signature, selected.returnValue) }}</span>
      </p>
      <div class="max-h-80 overflow-auto">
        <table class="w-full table-fixed text-xs">
          <thead class="text-left text-muted-foreground">
            <tr>
              <th class="w-20 px-2 py-1 font-normal">{{ t("plDebug.params.name") }}</th>
              <th class="w-16 px-2 py-1 font-normal">{{ t("plDebug.params.mode") }}</th>
              <th class="w-28 px-2 py-1 font-normal">{{ t("plDebug.params.type") }}</th>
              <th class="px-2 py-1 font-normal">{{ t("plDebug.params.value") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(param, index) in editableParams" :key="`${param.name}-${index}`" class="border-t">
              <td class="break-all px-2 py-1 align-top font-mono">{{ param.name }}</td>
              <td class="whitespace-nowrap px-2 py-1 align-top">{{ param.mode }}</td>
              <td class="max-w-36 px-2 py-1 align-top font-mono" :class="shapeFor(index) ? 'min-w-28 break-words' : ''" :title="selected ? argumentType(selected.signature, selected.parameters[index]!) : param.type">{{ shapeFor(index)?.reference?.typeName ?? param.type }}</td>
              <td class="min-w-0 px-2 py-1">
                <div v-if="param.mode !== 'OUT'" class="flex flex-wrap gap-1">
                  <select v-if="!shapeFor(index) || selected?.parameters[index]?.defaulted === 'Y'" v-model="param.inputState" class="h-8 rounded-md border bg-background px-1 text-sm" :disabled="starting" :aria-label="`${param.name}: ${t('plDebug.start.inputState')}`">
                    <option value="value">{{ t("plDebug.start.textValue") }}</option>
                    <option v-if="!shapeFor(index)" value="null">SQL NULL</option>
                    <option v-if="selected?.parameters[index]?.defaulted === 'Y'" value="default">{{ t("plDebug.start.defaultValue") }}</option>
                  </select>
                  <div v-if="param.inputState === 'value' && shapeFor(index) && selected && (param.typedValue?.kind === 'record' || param.typedValue?.kind === 'collection' || param.typedValue?.kind === 'object')" class="min-w-0 basis-full space-y-2 py-1">
                    <PlDebugCompositeEditor :model-value="param.typedValue" :signature="selected.signature" :shape="shapeFor(index)!" :label="param.name" :disabled="starting" @update:model-value="updateCompositeParam(param, $event)" />
                    <p v-if="!compositeComplete(param, index)" role="alert" class="text-sm text-destructive">{{ t("plDebug.start.compositeInvalid") }}</p>
                  </div>
                  <div v-else-if="param.inputState === 'value' && isJsonParamType(param.type)" class="min-w-0 basis-full space-y-1 py-1">
                    <PlDebugJsonEditor :model-value="param.value ?? ''" :label="param.name" :disabled="starting" :hint="jsonHint(param)" @update:model-value="param.value = String($event)" />
                    <Button v-if="sourceFields(param.name).length" variant="outline" size="sm" class="h-7 px-2 text-xs" :disabled="starting" @click="regenerateJSON(param)">
                      {{ t("plDebug.start.jsonRegenerate") }}
                    </Button>
                  </div>
                  <div v-else-if="param.inputState === 'value' && param.typedValue?.kind === 'timestamp_tz'" class="flex min-w-0 flex-1 flex-wrap gap-2">
                    <label class="min-w-48 flex-1 text-muted-foreground"
                      >{{ t("plDebug.start.localTime")
                      }}<Input :model-value="param.typedValue.value.local" class="mt-1 h-8 font-mono text-sm" :disabled="starting" :aria-label="`${param.name}: local`" placeholder="YYYY-MM-DDTHH:mm:ss.fffffffff" @update:model-value="updateTimezoneParam(param, 'local', $event)"
                    /></label>
                    <label class="w-24 text-muted-foreground"
                      >{{ t("plDebug.start.utcOffset") }}<Input :model-value="param.typedValue.value.offset" class="mt-1 h-8 font-mono text-sm" :disabled="starting" :aria-label="`${param.name}: offset`" placeholder="+00:00" @update:model-value="updateTimezoneParam(param, 'offset', $event)"
                    /></label>
                    <label class="min-w-40 flex-1 text-muted-foreground"
                      >{{ t("plDebug.start.optionalRegion")
                      }}<Input :model-value="param.typedValue.value.region ?? ''" class="mt-1 h-8 font-mono text-sm" :disabled="starting" :aria-label="`${param.name}: region`" placeholder="America/New_York" @update:model-value="updateTimezoneParam(param, 'region', $event)"
                    /></label>
                  </div>
                  <Input
                    v-else-if="param.inputState === 'value' && param.typedValue?.kind === 'timestamp_ltz'"
                    :model-value="param.typedValue.value.instant"
                    class="h-8 min-w-48 flex-1 font-mono text-sm"
                    :disabled="starting"
                    :aria-label="`${param.name}: instant`"
                    placeholder="YYYY-MM-DDTHH:mm:ss.fffffffffZ"
                    @update:model-value="updateTimezoneParam(param, 'instant', $event)"
                  />
                  <select v-else-if="param.inputState === 'value' && param.type === 'PL/SQL BOOLEAN'" v-model="param.value" class="h-8 rounded-md border bg-background px-2 font-mono text-sm" :disabled="starting" :aria-label="param.name">
                    <option value="false">false</option>
                    <option value="true">true</option>
                  </select>
                  <label v-else-if="param.inputState === 'value' && (param.type === 'CLOB' || param.type === 'NCLOB' || param.type === 'BLOB' || param.type === 'RAW')" class="min-w-48 flex-1">
                    <span v-if="param.type === 'BLOB' || param.type === 'RAW'" class="text-muted-foreground">Base64</span>
                    <textarea
                      :value="param.value ?? ''"
                      rows="3"
                      class="w-full resize-y rounded-md border bg-background px-2 py-1 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
                      :disabled="starting"
                      :aria-label="param.type === 'BLOB' || param.type === 'RAW' ? `${param.name}: Base64` : param.name"
                      spellcheck="false"
                      autocapitalize="off"
                      @input="param.value = ($event.target as HTMLTextAreaElement).value"
                    />
                  </label>
                  <Input
                    v-else-if="param.inputState === 'value'"
                    :model-value="param.value ?? ''"
                    class="h-8 min-w-24 flex-1 font-mono text-sm"
                    :disabled="starting"
                    :aria-label="param.name"
                    :placeholder="
                      param.type === 'BINARY_FLOAT' ? 'IEEE 754 hex: 3F800000' : param.type === 'BINARY_DOUBLE' ? 'IEEE 754 hex: 3FF0000000000000' : param.type === 'DATE' ? 'YYYY-MM-DDTHH:mm:ss' : param.type === 'TIMESTAMP' ? 'YYYY-MM-DDTHH:mm:ss.fffffffff' : t('plDebug.params.valuePlaceholder')
                    "
                    @update:model-value="param.value = String($event)"
                  />
                </div>
                <span v-else class="text-muted-foreground">OUT</span>
              </td>
            </tr>
            <tr v-if="loading && editableParams.length === 0">
              <td colspan="4" class="px-2 py-3 text-center text-muted-foreground"><Loader2 class="mr-1 inline h-3 w-3 animate-spin" />{{ t("plDebug.launch.loading") }}</td>
            </tr>
            <tr v-else-if="editableParams.length === 0 && (anonymous || selected)">
              <td colspan="4" class="px-2 py-3 text-center text-muted-foreground">{{ t("plDebug.params.empty") }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-if="previewError" role="alert" class="text-sm text-destructive">{{ previewError }}</p>
      <slot name="compilation" />
      <p v-if="selected?.signature.typeBindings?.length" class="text-sm text-muted-foreground">{{ t("plDebug.start.compositeInitializerWarning") }}</p>
      <div v-if="selected?.sourceAnalysis" class="rounded-md border bg-muted/20 p-2 text-xs">
        <p class="font-medium">{{ t("plDebug.start.sourceAnalysis") }}</p>
        <p v-if="!analysisFields.length" class="mt-1 text-muted-foreground">{{ selected.sourceAnalysis.message || t("plDebug.start.sourceAnalysisEmpty") }}</p>
        <ul v-else class="mt-1 space-y-0.5">
          <li v-for="field in analysisFields" :key="`${field.parameter}:${field.path}`" class="truncate" :title="field.evidence">
            <span class="font-mono">{{ field.parameter }} → {{ field.path }}</span>
            <span class="text-muted-foreground"> · {{ t("plDebug.start.sourceAnalysisLine", { line: field.line }) }}</span>
          </li>
        </ul>
      </div>
      <div v-if="generatedCode" class="space-y-1">
        <p class="text-sm font-medium">{{ t("plDebug.start.generatedCode") }}</p>
        <pre class="max-h-32 overflow-auto whitespace-pre-wrap break-all rounded-md border bg-muted/30 p-2 font-mono text-xs">{{ generatedCode }}</pre>
      </div>
      <p v-if="errorMessage" class="text-xs text-destructive">{{ errorMessage }}</p>
      <DialogFooter>
        <Button v-if="store.recoveryRequired || store.pendingStart" variant="outline" :disabled="starting" @click="recoverDebug">{{ t("plDebug.start.recover") }}</Button>
        <Button variant="outline" :disabled="starting" @click="open = false">
          {{ t("common.cancel") }}
        </Button>
        <Button :disabled="!canStart" @click="startDebug">
          <Loader2 v-if="starting" class="mr-1 h-4 w-4 animate-spin" />
          {{ t("plDebug.start.confirm") }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
