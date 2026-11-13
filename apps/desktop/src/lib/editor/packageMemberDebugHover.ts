import { hoverTooltip, type EditorView } from "@codemirror/view";

export interface PackageMemberDebugHoverTarget {
  name: string;
  kind: "PROCEDURE" | "FUNCTION";
}

/**
 * 包 DDL 编辑器里的成员名悬停提示：指针停在成员名上时浮出「调试」按钮。
 *
 * 返回值必须注册在 SQL 自身的悬停提示之前，否则函数名会先命中 SQL 文档提示。
 */
export function packageMemberDebugHover(getTargets: () => readonly PackageMemberDebugHoverTarget[] | undefined, getLabel: () => string, onDebug: (target: PackageMemberDebugHoverTarget) => void) {
  return hoverTooltip(
    (view: EditorView, pos: number) => {
      const targets = getTargets();
      if (!targets?.length) return null;
      const line = view.state.doc.lineAt(pos);
      const text = line.text;
      let from = Math.min(pos - line.from, text.length);
      let to = from;
      while (from > 0 && isIdentifierChar(text[from - 1])) from -= 1;
      while (to < text.length && isIdentifierChar(text[to])) to += 1;
      const word = text.slice(from, to);
      if (!word) return null;
      const target = targets.find((candidate) => candidate.name.toLowerCase() === word.toLowerCase());
      if (!target) return null;
      return {
        pos: line.from + from,
        end: line.from + to,
        above: true,
        create() {
          const dom = document.createElement("div");
          dom.className = "flex items-center gap-2 rounded-md border bg-popover px-2 py-1 text-xs shadow-md";
          const kind = document.createElement("span");
          kind.className = "font-mono text-[10px] uppercase text-muted-foreground";
          kind.textContent = target.kind;
          dom.appendChild(kind);
          const button = document.createElement("button");
          button.type = "button";
          button.className = "rounded px-1.5 py-0.5 font-medium text-primary hover:bg-accent";
          button.textContent = getLabel();
          button.addEventListener("click", () => onDebug(target));
          dom.appendChild(button);
          return { dom };
        },
      };
    },
    { hoverTime: 300 },
  );
}

function isIdentifierChar(char: string | undefined): boolean {
  return !!char && /[A-Za-z0-9_$#]/.test(char);
}
