/**
 * 包 DDL 页左侧对象大纲的来源：从包源码里解析 PROCEDURE / FUNCTION 声明。
 *
 * 只做展示与调试入口定位，不承担完整 PL/SQL 解析；签名截断到第一层括号结束，
 * 包规范与包体中的同名声明按「类型 + 名称 + 签名」去重。
 */
export interface PackageSourceMember {
  name: string;
  kind: "PROCEDURE" | "FUNCTION";
  /** 括号内的参数列表，可能为空字符串。 */
  signature: string;
  /** 1-based 行号，用于在编辑器中定位。 */
  line: number;
}

/** 包 DDL 页左侧大纲项：声明来自包头，定位行来自包体实现。 */
export interface PackageOutlineMember extends PackageSourceMember {
  /** 包体中实现所在行（1-based）；包体不可用或无对应实现时为空。 */
  bodyLine?: number;
}

const MEMBER_HEAD_RE = /\b(PROCEDURE|FUNCTION)\s+("[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)/gi;

export function parsePackageSourceMembers(source: string): PackageSourceMember[] {
  if (!source) return [];
  const cleaned = stripSqlComments(source);
  const result: PackageSourceMember[] = [];
  const seen = new Set<string>();
  const lineStarts = buildLineStarts(cleaned);
  MEMBER_HEAD_RE.lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = MEMBER_HEAD_RE.exec(cleaned)) !== null) {
    const kind = match[1].toUpperCase() as PackageSourceMember["kind"];
    const name = match[2].replace(/^"|"$/g, "");
    const signature = readParameterList(cleaned, match.index + match[0].length);
    const key = `${kind}\0${name.toLowerCase()}\0${normalizeSignature(signature)}`;
    if (seen.has(key)) continue;
    seen.add(key);
    result.push({ name, kind, signature, line: lineNumberAt(lineStarts, match.index) });
  }
  return result;
}

/**
 * 把 `--` 行注释和块注释替换为空格，但保留换行符，确保行号与原始源码一致。
 * 包源码的注释里常出现 `@name pkg.member` 等文本，不能参与成员匹配。
 */
function stripSqlComments(source: string): string {
  let result = "";
  let index = 0;
  while (index < source.length) {
    const char = source[index];
    const next = source[index + 1];
    if (char === "-" && next === "-") {
      while (index < source.length && source[index] !== "\n") {
        result += " ";
        index += 1;
      }
      continue;
    }
    if (char === "/" && next === "*") {
      result += "  ";
      index += 2;
      while (index < source.length && !(source[index] === "*" && source[index + 1] === "/")) {
        result += source[index] === "\n" ? "\n" : " ";
        index += 1;
      }
      if (index < source.length) {
        result += "  ";
        index += 2;
      }
      continue;
    }
    result += char;
    index += 1;
  }
  return result;
}

function buildLineStarts(source: string): number[] {
  const starts = [0];
  for (let index = 0; index < source.length; index += 1) {
    if (source[index] === "\n") starts.push(index + 1);
  }
  return starts;
}

function lineNumberAt(starts: number[], offset: number): number {
  let low = 0;
  let high = starts.length - 1;
  while (low < high) {
    const middle = Math.ceil((low + high) / 2);
    if (starts[middle] <= offset) low = middle;
    else high = middle - 1;
  }
  return low + 1;
}

/** 从声明头之后读取括号参数；跳过空白与注释，未找到括号时返回空串。 */
function readParameterList(source: string, from: number): string {
  let index = from;
  while (index < source.length && /\s/.test(source[index])) index += 1;
  if (source[index] !== "(") return "";
  const start = index + 1;
  let depth = 1;
  index += 1;
  while (index < source.length && depth > 0) {
    const char = source[index];
    if (char === "(") depth += 1;
    else if (char === ")") depth -= 1;
    index += 1;
  }
  return source
    .slice(start, depth === 0 ? index - 1 : index)
    .replace(/\s+/g, " ")
    .trim();
}

/**
 * 包头声明 + 包体实现 → DDL 页大纲。
 *
 * 包头决定左侧列出哪些对象；包体决定点击后跳到哪一行。包体为空时退化为
 * 包头声明行（此时编辑区显示包头）。包体非空但解析不到实现时返回空数组，
 * 避免把包头行号错误地用在包体文本上。
 */
export function buildPackageOutline(headerSource: string, bodySource: string): PackageOutlineMember[] {
  const header = parsePackageSourceMembers(headerSource);
  if (header.length === 0) return [];
  if (!bodySource.trim()) return header.map((member) => ({ ...member }));
  const body = parsePackageSourceMembers(bodySource);
  if (body.length === 0) return [];
  const used = new Set<number>();
  const outline: PackageOutlineMember[] = [];
  for (const member of header) {
    const index = findBodyMember(body, member, used);
    if (index < 0) continue;
    used.add(index);
    outline.push({ ...member, bodyLine: body[index].line });
  }
  return outline;
}

function findBodyMember(body: readonly PackageSourceMember[], member: PackageSourceMember, used: Set<number>): number {
  const name = member.name.toLowerCase();
  let fallback = -1;
  for (let index = 0; index < body.length; index += 1) {
    if (used.has(index)) continue;
    const candidate = body[index];
    if (candidate.kind !== member.kind || candidate.name.toLowerCase() !== name) continue;
    if (normalizeSignature(candidate.signature) === normalizeSignature(member.signature)) return index;
    if (fallback < 0) fallback = index;
  }
  return fallback;
}

/**
 * 包头与包体的参数写法不完全一致：包头会带 `IN`/`OUT` 模式和 `DEFAULT`，
 * 包体实现通常省略。归一化后再比较，重载才不会被误判成同一成员。
 */
function normalizeSignature(signature: string): string {
  return signature
    .replace(/\s+/g, " ")
    .toLowerCase()
    .replace(/\bin\s+out\b/g, " ")
    .replace(/\b(in|out)\b/g, " ")
    .replace(/\bdefault\b[^,]*/g, " ")
    .replace(/:=/g, " ")
    .replace(/=[^,]*/g, " ")
    .replace(/\s+/g, " ")
    .trim();
}

/** 新建包子对象时用户填写的内容。 */
export interface PackageMemberDefinition {
  kind: "PROCEDURE" | "FUNCTION";
  name: string;
  /** 括号内的参数列表，不含括号，可为空。 */
  parameters: string;
  /** FUNCTION 的返回类型；PROCEDURE 忽略。 */
  returnType?: string;
}

export interface PackageSourcePair {
  headerSource: string;
  bodySource: string;
}

/** 1-based 行号 → 行首偏移。 */
function lineStartOffset(source: string, line: number): number {
  let offset = 0;
  for (let current = 1; current < line; current += 1) {
    const next = source.indexOf("\n", offset);
    if (next < 0) return source.length;
    offset = next + 1;
  }
  return offset;
}

/** 从包头声明开头扫描到第一层分号（括号内的分号不算）。 */
function declarationEndOffset(source: string, start: number): number {
  const cleaned = stripSqlComments(source);
  let depth = 0;
  for (let index = start; index < cleaned.length; index += 1) {
    const char = cleaned[index];
    if (char === "(") depth += 1;
    else if (char === ")") depth = Math.max(0, depth - 1);
    else if (char === ";" && depth === 0) return index + 1;
  }
  return cleaned.length;
}

/**
 * 从包体实现开头扫描到匹配的 `END [name];`。PL/SQL 的 IF/LOOP/CASE 也以 END
 * 结束，按块深度配对；`END IF`/`END LOOP` 里的关键字不重复计数。
 */
function implementationEndOffset(source: string, start: number): number {
  const cleaned = stripSqlComments(source);
  const tokenPattern = /\b(BEGIN|IF|LOOP|CASE|END)\b/gi;
  tokenPattern.lastIndex = start;
  let depth = 0;
  let inBody = false;
  let match: RegExpExecArray | null;
  while ((match = tokenPattern.exec(cleaned)) !== null) {
    const token = match[1].toUpperCase();
    if (token === "END") {
      if (!inBody) continue;
      depth -= 1;
      if (depth === 0) {
        const semicolon = cleaned.indexOf(";", tokenPattern.lastIndex);
        return semicolon < 0 ? cleaned.length : semicolon + 1;
      }
      continue;
    }
    if (token === "BEGIN") {
      inBody = true;
      depth = 1;
      continue;
    }
    // 声明行到第一个 BEGIN 之间的 IF/LOOP/CASE 属于表达式或类型名，不计块。
    if (!inBody) continue;
    const before = cleaned.slice(Math.max(0, match.index - 12), match.index).trimEnd();
    if (/\bEND$/i.test(before)) continue;
    depth += 1;
  }
  return declarationEndOffset(source, start);
}

/** 删除从行首到语句结束（含换行）的整段文本。 */
function removeStatement(source: string, line: number, endOf: (source: string, start: number) => number): string {
  const start = lineStartOffset(source, line);
  let end = endOf(source, start);
  while (end < source.length && (source[end] === " " || source[end] === "\t" || source[end] === "\r")) end += 1;
  if (source[end] === "\n") end += 1;
  return `${source.slice(0, start)}${source.slice(end)}`;
}

/**
 * 在包的最后一个 `END [name];` 之前插入成员块，保持 END 仍是文件的最后一条语句。
 *
 * 兼容 Oracle/PL/SQL 脚本里常见的末尾 `/` 执行标记：把它视为 END 之后的尾随内容，
 * 新成员仍然插到 END 之前，否则会被追加到 `/` 之后形成非法 DDL。
 */
function insertBeforeFinalEnd(source: string, block: string): string {
  const match = /\bEND\b[^;]*;\s*(\/\s*)?$/i.exec(source);
  if (!match) return `${source.replace(/\s*$/, "")}\n${block}\n`;
  const lineStart = source.lastIndexOf("\n", match.index - 1) + 1;
  const before = source.slice(0, lineStart);
  // 上一行不是空行时补一个空行，让新成员与前一个成员/声明之间有视觉分隔。
  const trimmedBefore = before.replace(/\n$/, "");
  const separator = /\n\s*$/.test(trimmedBefore) ? "" : "\n";
  return `${before}${separator}${block}\n${source.slice(lineStart)}`;
}

function memberMatches(candidate: PackageSourceMember, member: Pick<PackageSourceMember, "name" | "kind" | "signature">): boolean {
  return candidate.kind === member.kind && candidate.name.toLowerCase() === member.name.toLowerCase() && normalizeSignature(candidate.signature) === normalizeSignature(member.signature);
}

/**
 * 在包头声明和包体实现中同时新增一个成员。包体里生成可直接编译的空实现，
 * 用户随后在编辑区补全逻辑即可。
 */
export function addPackageMemberToSources(headerSource: string, bodySource: string, member: PackageMemberDefinition): PackageSourcePair {
  const name = member.name.trim();
  const parameters = member.parameters.trim();
  const returnType = (member.returnType ?? "").trim();
  const header = member.kind === "FUNCTION" ? `  FUNCTION ${name}(${parameters}) RETURN ${returnType};` : `  PROCEDURE ${name}(${parameters});`;
  const body = member.kind === "FUNCTION" ? `  FUNCTION ${name}(${parameters}) RETURN ${returnType} IS\n  BEGIN\n    RETURN NULL;\n  END ${name};` : `  PROCEDURE ${name}(${parameters}) IS\n  BEGIN\n    NULL;\n  END ${name};`;
  return {
    headerSource: insertBeforeFinalEnd(headerSource, header),
    bodySource: insertBeforeFinalEnd(bodySource, body),
  };
}

/** 从包头声明和包体实现中同时删除一个成员。 */
export function removePackageMemberFromSources(headerSource: string, bodySource: string, member: Pick<PackageSourceMember, "name" | "kind" | "signature">): PackageSourcePair {
  const headerMember = parsePackageSourceMembers(headerSource).find((candidate) => memberMatches(candidate, member));
  const bodyMember = parsePackageSourceMembers(bodySource).find((candidate) => memberMatches(candidate, member));
  return {
    headerSource: headerMember ? removeStatement(headerSource, headerMember.line, declarationEndOffset) : headerSource,
    bodySource: bodyMember ? removeStatement(bodySource, bodyMember.line, implementationEndOffset) : bodySource,
  };
}

/** 按名称（大小写不敏感）查找大纲成员，供“再次点击包成员定位”使用。 */
export function findPackageOutlineMemberByName(members: readonly PackageOutlineMember[], name: string): PackageOutlineMember | undefined {
  const target = name.trim().replace(/^"|"$/g, "").toLowerCase();
  return members.find((member) => member.name.toLowerCase() === target);
}

/** 返回包体中包含该行的成员（取实现行不晚于该行的最近一个）。 */
export function packageOutlineMemberAtLine(members: readonly PackageOutlineMember[], line: number): PackageOutlineMember | undefined {
  let match: PackageOutlineMember | undefined;
  for (const member of members) {
    if (member.bodyLine === undefined || member.bodyLine > line) continue;
    if (!match || member.bodyLine > (match.bodyLine ?? 0)) match = member;
  }
  return match;
}
