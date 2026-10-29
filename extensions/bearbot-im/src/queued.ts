import type { Context } from "@earendil-works/pi-ai";
import type { ExtensionContext } from "@earendil-works/pi-coding-agent";
import type { InboundMessage } from "../../../sdk/index.ts";
import { assistantText, renderObservedLine } from "./render.ts";

// Summary of the queued messages of other senders shown in a turn (DESIGN 7.2, 7.3).

/** Upper bound of one summary request; the turn waits for it before it is dispatched. */
export const QUEUED_SUMMARY_TIMEOUT_MS = 60_000;

export type QueuedSummary = { kind: "summary"; text: string } | { kind: "failed"; error: string };

/** The user message of the summary request: the queued messages as observed lines (escaped) inside `<queued>`. */
export function buildQueuedSummaryRequest(queued: InboundMessage[], refDate: string): string {
  return ["<queued>", ...queued.map((message) => renderObservedLine(message, refDate)), "</queued>", "请按说明写出摘要。"].join("\n");
}

/** One summary through ctx.modelRegistry with the session's current model. */
export async function summarizeQueued(
  ctx: ExtensionContext,
  systemPrompt: string,
  queued: InboundMessage[],
  refDate: string,
  timeoutMs: number,
): Promise<QueuedSummary> {
  const model = ctx.model;
  if (!model) return { kind: "failed", error: "agent 当前没有选择模型" };
  const context: Context = {
    systemPrompt,
    messages: [{ role: "user", content: [{ type: "text", text: buildQueuedSummaryRequest(queued, refDate) }], timestamp: Date.now() }],
  };
  const timeout = AbortSignal.timeout(timeoutMs);
  const message = await ctx.modelRegistry.complete(model, context, { signal: timeout, maxTokens: 2048 });
  if (timeout.aborted) return { kind: "failed", error: `模型超过 ${timeoutMs} 毫秒没有给出摘要` };
  if (message.stopReason === "error" || message.stopReason === "aborted") {
    return { kind: "failed", error: message.errorMessage ?? `模型调用以 ${message.stopReason} 结束` };
  }
  const text = assistantText(message.content).trim();
  if (text === "") return { kind: "failed", error: "模型没有给出摘要文字" };
  return { kind: "summary", text };
}
