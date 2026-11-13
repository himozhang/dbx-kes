import type { SqlCompletionScope } from "@/lib/sql/sqlCompletionLookupTarget";
import type { DatabaseType, QueryResult } from "@/types/database";
import type { CodeMirrorSqlDialectName } from "@/lib/editor/codemirrorSqlDialect";
import type { SqlFormatDialect } from "@/lib/sql/sqlFormatter";
import type { StatementExecutionMarker } from "@/lib/tabs/tabPresentation";
import type { MongoQueryLanguage } from "@/lib/mongo/mongoSql";

export interface QueryEditorProps {
  modelValue: string;
  /** Identity of the tab owning the document. Changing it swaps in that tab's cached editor state (fresh undo history on first visit). */
  tabId?: string;
  connectionId?: string;
  catalog?: string;
  database?: string;
  /** Warm table metadata for the active database; only query tabs enable this. */
  prefetchCompletion?: boolean;
  schema?: string;
  clientSessionId?: string;
  completionContextVersion?: number;
  databaseType?: DatabaseType;
  dialect?: "mysql" | "postgres" | "sqlserver";
  syntaxDialect?: CodeMirrorSqlDialectName;
  formatDialect?: SqlFormatDialect;
  formatRequestId?: number;
  compressRequestId?: number;
  /** Toolbar `a`/`A`: convert keywords in place, matching `keywordCaseRequestMode`. */
  keywordCaseRequestId?: number;
  keywordCaseRequestMode?: "upper" | "lower";
  executionError?: string;
  executionErrorSql?: string;
  executionErrorResult?: QueryResult;
  resultColumns?: string[];
  resultSourceStatement?: string;
  resultSourceFrom?: number;
  resultSourceTo?: number;
  readOnly?: boolean;
  autoFocus?: boolean;
  forceWordWrap?: boolean;
  hideExecutionControls?: boolean;
  enableExplainShortcut?: boolean;
  canExplain?: boolean;
  isExecuting?: boolean;
  initialViewport?: { scrollTop: number; scrollLeft: number };
  initialSelection?: { anchor: number; head: number };
  revealRequest?: { id: number; line: number; column?: number };
  statementExecutionMarkers?: StatementExecutionMarker[];
  /** MongoDB query tabs: SQL mode uses the SQL statement splitter and formatter. */
  mongoQueryLanguage?: MongoQueryLanguage;
  /** 包 DDL：可悬停调试的成员名（浮出「调试」按钮）。 */
  debugHoverMembers?: Array<{ name: string; kind: "PROCEDURE" | "FUNCTION" }>;
  /** 悬停浮层中调试按钮的文案。 */
  debugHoverLabel?: string;
  /** PL/SQL 函数/存储过程 DDL 页：允许点击行号左侧轨道切换断点。 */
  breakpointsEnabled?: boolean;
  /** 当前已设置的断点行号（1-based），由 plDebugStore 的待生效断点提供。 */
  breakpointLines?: number[];
}

export type CompletionMetadataScope = Pick<SqlCompletionScope, "database" | "schema">;
