import { RangeSet, StateEffect, StateField, type EditorState, type Extension, type Range } from "@codemirror/state";
import { EditorView, GutterMarker, gutter } from "@codemirror/view";

/**
 * DDL 编辑器（QueryEditor）中的断点轨道。
 *
 * 调试会话开始前，用户可以在函数/存储过程的 DDL 页按行号左侧的轨道设置断点；
 * 这些行号由父组件持有（plDebugStore 的 pending breakpoints），本扩展只负责
 * 画点和把点击换算成 1-based 行号，不缓存任何会话状态。
 */

/** 父组件行号列表变化后派发，用于整表重画（与 PL/SQL 调试编辑器同一策略）。 */
export const refreshQueryEditorBreakpointsEffect = StateEffect.define<null>();

class QueryEditorBreakpointMarker extends GutterMarker {
  toDOM() {
    const dot = document.createElement("span");
    dot.className = "cm-query-editor-breakpoint-dot";
    return dot;
  }
}

const breakpointMarker = new QueryEditorBreakpointMarker();

function buildMarkerSet(state: EditorState, getLines: () => readonly number[]): RangeSet<GutterMarker> {
  const ranges: Array<Range<GutterMarker>> = [];
  const seen = new Set<number>();
  for (const lineNum of getLines()) {
    if (!Number.isFinite(lineNum) || lineNum < 1 || lineNum > state.doc.lines) continue;
    const from = state.doc.line(lineNum).from;
    if (seen.has(from)) continue;
    seen.add(from);
    ranges.push(breakpointMarker.range(from));
  }
  return ranges.length > 0 ? RangeSet.of(ranges, true) : RangeSet.empty;
}

export interface QueryEditorBreakpointOptions {
  /** 1-based 行号，每次刷新时重新读取。 */
  getLines: () => readonly number[];
  /** 点击轨道后切换该行断点。 */
  toggleBreakpoint: (lineNum: number) => void;
}

export function createQueryEditorBreakpointExtension(options: QueryEditorBreakpointOptions): Extension {
  const field = StateField.define<RangeSet<GutterMarker>>({
    create: (state) => buildMarkerSet(state, options.getLines),
    update: (value, transaction) => {
      const refresh = transaction.docChanged || transaction.effects.some((effect) => effect.is(refreshQueryEditorBreakpointsEffect));
      return refresh ? buildMarkerSet(transaction.state, options.getLines) : value;
    },
  });
  const breakpointGutter = gutter({
    class: "cm-query-editor-breakpoint-gutter",
    markers: (editorView) => editorView.state.field(field),
    initialSpacer: () => breakpointMarker,
    domEventHandlers: {
      mousedown(editorView, line, event) {
        if (event instanceof MouseEvent && event.button !== 0) return false;
        options.toggleBreakpoint(editorView.state.doc.lineAt(line.from).number);
        return true;
      },
    },
  });
  const theme = EditorView.theme({
    ".cm-query-editor-breakpoint-gutter": { width: "12px", cursor: "pointer" },
    ".cm-query-editor-breakpoint-gutter .cm-gutterElement": { display: "flex", alignItems: "center", justifyContent: "center", padding: "0" },
    ".cm-query-editor-breakpoint-dot": { display: "inline-block", width: "8px", height: "8px", borderRadius: "4px", background: "#ff4d4f" },
  });
  return [field, breakpointGutter, theme];
}
