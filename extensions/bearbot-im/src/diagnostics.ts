import { randomUUID } from "node:crypto";
import { appendFileSync, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { escapeText, localIso } from "./render.ts";

export type DiagnosticLevel = "error" | "warning";

export interface DiagnosticRecord {
  id: string;
  time: string;
  source: string;
  level: DiagnosticLevel;
  message: string;
  detail?: string;
  delivered: boolean;
}

export interface DiagnosticInput {
  source: string;
  level: DiagnosticLevel;
  message: string;
  detail?: string;
  /** false for diagnostics whose admins are already told by someone else (CLASSIFIER.md restores by the daemon). */
  notifyAdmins?: boolean;
}

/** Sends an error diagnostic to the admins; installed once the gateway is connected. */
export type AdminNotifier = (record: DiagnosticRecord) => void;

export const ADMIN_NOTIFY_INTERVAL_MS = 60 * 60 * 1000;
const KEPT_DELIVERED = 500;
const DETAIL_LIMIT_IN_TURN = 4000;

export function diagnosticsFile(workspaceDir: string): string {
  return join(workspaceDir, ".bearbot", "diagnostics.jsonl");
}

/**
 * Diagnostics store (DESIGN 7.8). In normal mode every record is appended to
 * `<workspace>/.bearbot/diagnostics.jsonl`; undelivered records go into the next turn. In trial mode (file null)
 * records are written to stderr so that they end up in the check report.
 */
export class Diagnostics {
  private readonly records: DiagnosticRecord[] = [];
  private notifier: AdminNotifier | null = null;
  private readonly lastNotified = new Map<string, number>();

  constructor(private readonly file: string | null) {
    if (file === null) return;
    mkdirSync(dirname(file), { recursive: true });
    if (!existsSync(file)) return;
    for (const line of readFileSync(file, "utf8").split("\n")) {
      if (line.trim() === "") continue;
      this.records.push(JSON.parse(line) as DiagnosticRecord);
    }
  }

  setNotifier(notifier: AdminNotifier): void {
    this.notifier = notifier;
  }

  add(input: DiagnosticInput): DiagnosticRecord {
    const record: DiagnosticRecord = {
      id: randomUUID(),
      time: localIso(new Date()),
      source: input.source,
      level: input.level,
      message: input.message,
      ...(input.detail === undefined ? {} : { detail: input.detail }),
      delivered: false,
    };
    if (this.file === null) {
      const detail = record.detail === undefined ? "" : `\n${record.detail}`;
      process.stderr.write(`[bearbot 诊断 ${record.level}] ${record.source}：${record.message}${detail}\n`);
      return record;
    }
    this.records.push(record);
    appendFileSync(this.file, `${JSON.stringify(record)}\n`);
    if (record.level === "error" && input.notifyAdmins !== false) this.maybeNotify(record);
    return record;
  }

  pending(): DiagnosticRecord[] {
    return this.records.filter((record) => !record.delivered);
  }

  tail(n: number): DiagnosticRecord[] {
    return this.records.slice(-n);
  }

  /** Marks records as delivered and rewrites the file, keeping the newest delivered records for bearbot_logs. */
  markDelivered(ids: string[]): void {
    if (this.file === null || ids.length === 0) return;
    const set = new Set(ids);
    for (const record of this.records) if (set.has(record.id)) record.delivered = true;
    let deliveredToDrop = this.records.filter((record) => record.delivered).length - KEPT_DELIVERED;
    for (let i = 0; i < this.records.length && deliveredToDrop > 0;) {
      if (this.records[i].delivered) {
        this.records.splice(i, 1);
        deliveredToDrop -= 1;
      } else {
        i += 1;
      }
    }
    const temp = `${this.file}.${process.pid}.tmp`;
    writeFileSync(temp, this.records.map((record) => `${JSON.stringify(record)}\n`).join(""));
    renameSync(temp, this.file);
  }

  private maybeNotify(record: DiagnosticRecord): void {
    if (!this.notifier) return;
    const now = Date.now();
    const last = this.lastNotified.get(record.source);
    if (last !== undefined && now - last < ADMIN_NOTIFY_INTERVAL_MS) return;
    this.lastNotified.set(record.source, now);
    this.notifier(record);
  }
}

/** Text of the `bearbot-diagnostics` message attached to a turn. */
export function formatDiagnosticsForTurn(records: DiagnosticRecord[]): string {
  const lines = [
    "<bearbot-diagnostics>",
    "以下是上一轮之后产生的诊断信息，请优先处理（admin 回合中可以直接修复；member 回合中需要修改文件时，告诉用户需要管理员处理）：",
  ];
  for (const record of records) {
    lines.push(`[${record.time}] ${record.level} 来源 ${escapeText(record.source)}：${escapeText(record.message)}`);
    if (record.detail !== undefined && record.detail !== "") {
      const detail = record.detail.length > DETAIL_LIMIT_IN_TURN
        ? `${record.detail.slice(0, DETAIL_LIMIT_IN_TURN)}\n……（已截断，完整内容见 bearbot_logs 的 diagnostics）`
        : record.detail;
      lines.push(escapeText(detail));
    }
  }
  lines.push("</bearbot-diagnostics>");
  return lines.join("\n");
}

/** Text sent to the admins for an error diagnostic. */
export function formatAdminNotice(record: DiagnosticRecord): string {
  const detail = record.detail === undefined ? "" : `\n${record.detail.slice(0, 1500)}`;
  return `bearbot 诊断（${record.level}）\n来源：${record.source}\n${record.message}${detail}\n同一来源每小时最多通知一次，完整内容会在下一轮交给 agent。`;
}
