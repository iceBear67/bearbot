import type { AgentState } from "./agent-state.ts";
import { errorText, type PinoLogger } from "./log.ts";
import type { Outbound, PostContent } from "./outbound.ts";
import type { Repo } from "./repo.ts";
import { RpcError } from "./rpc.ts";

export type BtwParams = {
  question: string;
  thread: string;
  sender: { identity: string; name: string };
};

/** Sends the `btw` request to bearbot-im (implemented by the bridge server). */
export interface BtwAsker {
  /** null when no bearbot-im connection has completed hello. */
  askBtw(params: BtwParams): Promise<unknown> | null;
}

export type BtwContext = {
  /** Thread the question came from; the answer goes there. */
  threadId: string;
  sender: { identity: string; name: string };
  /** Text after "!bb". */
  question: string;
};

export const BTW_USAGE = "用法：!bb <问题>，在不打断 agent 的情况下询问它正在做什么";

const TYPING_INTERVAL_MS = 5000;

/** A user error; the message is the reply. */
class BtwError extends Error {}

/** `!bb <问题>` (DESIGN 6.9). Only admins get an answer. */
export class Btw {
  constructor(
    private readonly deps: {
      repo: Repo;
      outbound: Outbound;
      agentState: AgentState;
      asker: BtwAsker;
      logger: PinoLogger;
    },
  ) {}

  async run(ctx: BtwContext): Promise<void> {
    const { repo, outbound, logger } = this.deps;
    if (!repo.isAdmin(ctx.sender.identity)) {
      logger.info({ sender: ctx.sender.identity, thread: ctx.threadId }, "!bb from a non-admin ignored");
      return;
    }
    let reply: PostContent;
    try {
      reply = { markdown: await this.ask(ctx) };
    } catch (e) {
      if (e instanceof BtwError) {
        reply = { text: e.message };
      } else if (e instanceof RpcError) {
        reply = { text: `btw 提问失败：${e.message}` };
      } else {
        logger.error({ err: e, thread: ctx.threadId }, "!bb failed");
        reply = { text: `btw 提问失败：${errorText(e)}` };
      }
    }
    try {
      await outbound.post(ctx.threadId, reply);
    } catch (e) {
      logger.error({ err: e, thread: ctx.threadId }, "sending the !bb reply failed");
    }
  }

  private async ask(ctx: BtwContext): Promise<string> {
    if (ctx.question === "") {
      throw new BtwError(BTW_USAGE);
    }
    const pending = this.deps.asker.askBtw({ question: ctx.question, thread: ctx.threadId, sender: ctx.sender });
    if (pending === null) {
      throw new BtwError(`bearbot-im 没有连接 gateway（agent 状态：${this.deps.agentState.state ?? "未知"}），无法回答`);
    }
    const typing = this.keepTyping(ctx.threadId);
    try {
      const result = await pending;
      const answer = (result as { answer?: unknown } | null)?.answer;
      if (typeof answer !== "string" || answer.trim() === "") {
        throw new Error(`bearbot-im 的响应中没有回答：${JSON.stringify(result)}`);
      }
      return answer;
    } finally {
      clearInterval(typing);
    }
  }

  /** The typing indicator while bearbot-im answers; a failure is logged once. */
  private keepTyping(threadId: string): NodeJS.Timeout {
    let warned = false;
    const send = (): void => {
      this.deps.outbound.typing(threadId).catch((e: unknown) => {
        if (warned) return;
        warned = true;
        this.deps.logger.warn({ err: e, thread: threadId }, "typing for !bb failed");
      });
    };
    send();
    return setInterval(send, TYPING_INTERVAL_MS);
  }
}
