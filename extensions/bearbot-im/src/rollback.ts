import type { RollbackRecord } from "./agent-client.ts";

// Admin rollback handling (DESIGN 9.6).

function requesterText(record: RollbackRecord): string {
  if (record.requesters === null || record.requesters.length === 0) return "（通过命令行）";
  return record.requesters.join("、");
}

function revertedText(record: RollbackRecord): string {
  if (record.reverted.length === 0) return "无";
  return record.reverted.map((item) => `${item.ref}：${item.summary}`).join("；");
}

/** Body lines of the `kind="rollback"` system turn (9.6 step 1). */
export function rollbackTurnBody(record: RollbackRecord): string[] {
  return [
    `管理员 ${requesterText(record)} 把 bot 回滚到了快照 ${record.target}，回滚之前的状态保存在 ${record.saved}。`,
    `被撤销的应用：${revertedText(record)}`,
    "AGENTS.md 与 CLASSIFIER.md 保持回滚之前的内容。",
    "与被撤销的改动有关的任务现在暂停：不要重新实现或重新应用这些改动，也不要继续由它们开始的工作，等待管理员的指示。",
    "请把这次回滚写入 AGENTS.md 的“经验记录”，并告诉管理员你目前的状态、暂停了哪些任务。",
  ];
}

/** Reminder appended to the bearbot system prompt section while the record exists (9.6 step 2). */
export function rollbackReminder(record: RollbackRecord): string {
  return [
    "## 回滚提醒",
    "",
    `管理员 ${requesterText(record)} 把 bot 回滚到了快照 ${record.target}，回滚之前的状态保存在 ${record.saved}。被撤销的应用：${revertedText(record)}。`,
    "与被撤销的改动有关的任务仍然暂停：不要重新实现或重新应用这些改动，也不要继续由它们开始的工作，等待管理员的指示。与被撤销的改动无关的消息照常处理。",
    "管理员在 IM 中发起的回合正常结束后，这条提醒才会解除。",
  ].join("\n");
}

export interface FinishedTurn {
  role: "admin" | "member" | "system";
  viaPrompt: boolean;
  /** Every trigger message came from an admin in the IM (the console operator counts). */
  allSendersAdmins: boolean;
  messageCount: number;
  aborted: boolean;
  error: string | undefined;
}

/** Conditions of 9.6 step 3 for POST /v1/rollback/ack. */
export function rollbackAckAllowed(turn: FinishedTurn): boolean {
  return turn.role === "admin"
    && !turn.viaPrompt
    && turn.messageCount > 0
    && turn.allSendersAdmins
    && !turn.aborted
    && turn.error === undefined;
}

/** The unacknowledged rollback record, as last returned by bridge/ready. */
export class RollbackState {
  private current: RollbackRecord | null = null;

  get record(): RollbackRecord | null {
    return this.current;
  }

  set(record: RollbackRecord | null): void {
    this.current = record;
  }

  clear(id: number): void {
    if (this.current?.id === id) this.current = null;
  }
}
