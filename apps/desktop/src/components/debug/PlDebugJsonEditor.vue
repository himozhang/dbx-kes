<script setup lang="ts">
import { computed, ref } from "vue";
import { useI18n } from "vue-i18n";
import { AlertCircle, Check, Wand2 } from "@lucide/vue";

/**
 * 紧凑的 JSON 编辑器：行号 + 自动展开高度 + 格式化 + 语法校验。
 *
 * json/jsonb 参数用它替代普通输入框；默认就是展开状态，不藏在按钮后面。
 */
const { t } = useI18n();

const props = withDefaults(
  defineProps<{
    modelValue: string;
    disabled?: boolean;
    /** 无障碍标签，通常传参数名。 */
    label?: string;
    /** 生成来源提示，例如“已按源码引用的 3 个字段生成”。 */
    hint?: string;
    placeholder?: string;
    minRows?: number;
  }>(),
  { minRows: 4 },
);

const emit = defineEmits<{ "update:modelValue": [value: string] }>();

const gutter = ref<HTMLElement | null>(null);
const textarea = ref<HTMLTextAreaElement | null>(null);

const value = computed(() => props.modelValue ?? "");
const lines = computed(() => value.value.split("\n").length);
const rows = computed(() => Math.min(Math.max(lines.value, props.minRows), 16));
const validation = computed(() => {
  const text = value.value.trim();
  if (text === "") return { state: "empty" as const, message: t("plDebug.start.jsonEmpty") };
  try {
    JSON.parse(text);
    return { state: "valid" as const, message: t("plDebug.start.jsonValid") };
  } catch (error) {
    return { state: "invalid" as const, message: t("plDebug.start.jsonInvalid", { error: String((error as Error)?.message ?? error) }) };
  }
});

function update(next: string) {
  emit("update:modelValue", next);
}

function format() {
  try {
    update(JSON.stringify(JSON.parse(value.value), null, 2));
  } catch {
    // 非法 JSON 保持原文，校验提示已经说明原因。
  }
}

function syncScroll() {
  if (gutter.value && textarea.value) gutter.value.scrollTop = textarea.value.scrollTop;
}
</script>

<template>
  <div class="min-w-0 flex-1 overflow-hidden rounded-md border bg-background" :class="validation.state === 'invalid' ? 'border-destructive' : ''">
    <div class="flex items-center justify-between gap-2 border-b bg-muted/40 px-2 py-1">
      <span class="truncate text-xs font-medium text-muted-foreground">JSON</span>
      <div class="flex items-center gap-2">
        <span class="flex items-center gap-1 text-xs" :class="validation.state === 'invalid' ? 'text-destructive' : 'text-muted-foreground'">
          <AlertCircle v-if="validation.state === 'invalid'" class="h-3 w-3" />
          <Check v-else-if="validation.state === 'valid'" class="h-3 w-3" />
          <span class="max-w-64 truncate" :title="validation.message">{{ validation.message }}</span>
        </span>
        <button type="button" class="flex items-center gap-1 rounded px-1.5 py-0.5 text-xs text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-50" :disabled="disabled || validation.state !== 'valid'" :title="t('plDebug.start.jsonFormat')" @click="format">
          <Wand2 class="h-3 w-3" />{{ t("plDebug.start.jsonFormat") }}
        </button>
      </div>
    </div>
    <div class="flex min-w-0">
      <div ref="gutter" aria-hidden="true" class="max-h-72 shrink-0 select-none overflow-hidden border-r bg-muted/20 px-1.5 py-1 text-right font-mono text-xs leading-5 text-muted-foreground">
        <div v-for="line in Math.max(lines, minRows)" :key="line">{{ line }}</div>
      </div>
      <textarea
        ref="textarea"
        :value="value"
        :rows="rows"
        :disabled="disabled"
        :aria-label="label"
        class="max-h-72 min-h-20 min-w-0 flex-1 resize-y overflow-auto bg-transparent px-2 py-1 font-mono text-sm leading-5 outline-none disabled:opacity-50"
        spellcheck="false"
        autocapitalize="off"
        :placeholder="placeholder ?? '{}'"
        @input="update(($event.target as HTMLTextAreaElement).value)"
        @scroll="syncScroll"
      />
    </div>
    <p v-if="hint" class="border-t bg-muted/20 px-2 py-1 text-xs text-muted-foreground">{{ hint }}</p>
  </div>
</template>
