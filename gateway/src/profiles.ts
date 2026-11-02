import {
  NetworkError,
  PermissionError,
  ResourceNotFoundError,
  ValidationError,
  downloadAttachment,
} from "@chat-adapter/shared";
import type { UserInfo } from "chat";
import type { GatewayConfig } from "./config.ts";
import { safeFileName } from "./console-adapter.ts";
import { saveInboxFile } from "./ingest.ts";
import { errorText, type PinoLogger } from "./log.ts";
import type { MessageLog } from "./message-log.ts";
import type { GatewayTelegramAdapter, Platforms } from "./platforms.ts";
import { CONSOLE_OPERATOR } from "./repo.ts";
import { APP_ERROR, INVALID_PARAMS, RpcError } from "./rpc.ts";
import { localDate } from "./time.ts";
import type { ProfileInfo, WireAvatar, WireUserInfo } from "./types.ts";

/** QQ avatars are served for a bot AppID and the user OpenID (DESIGN 6.2.2). */
export function qqAvatarUrl(appId: string, openid: string): string {
  return `https://q.qlogo.cn/qqapp/${encodeURIComponent(appId)}/${encodeURIComponent(openid)}/640`;
}

/**
 * qlogo answers a missing avatar with HTTP 200 and a fixed placeholder; the real data is announced in `X-Info`
 * (DESIGN 6.2.2). Thrown from `onResponse`, so the placeholder body is not read.
 */
class QqNoAvatar extends Error {}

/** Extension and MIME type from the first bytes; avatars are only accepted when they are one of these images. */
function imageType(data: Buffer): { ext: string; mimeType: string } | null {
  if (data.length >= 8 && data.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]))) {
    return { ext: "png", mimeType: "image/png" };
  }
  if (data.length >= 3 && data[0] === 0xff && data[1] === 0xd8 && data[2] === 0xff) {
    return { ext: "jpg", mimeType: "image/jpeg" };
  }
  if (data.length >= 6 && data.subarray(0, 4).toString("latin1") === "GIF8") {
    return { ext: "gif", mimeType: "image/gif" };
  }
  if (data.length >= 12 && data.subarray(0, 4).toString("latin1") === "RIFF" && data.subarray(8, 12).toString("latin1") === "WEBP") {
    return { ext: "webp", mimeType: "image/webp" };
  }
  return null;
}

/** Telegram answers 400 for an unknown chat or user; other failures stay visible. */
function telegramNotFound(e: unknown): boolean {
  return e instanceof ValidationError || e instanceof ResourceNotFoundError || e instanceof PermissionError;
}

/** Profile and avatar of a sender identity (DESIGN 6.8 `user`, `avatar`). */
export class UserProfiles {
  constructor(
    private readonly deps: {
      cfg: GatewayConfig;
      platforms: Platforms;
      log: MessageLog;
      logger: PinoLogger;
    },
  ) {}

  /** "<平台>:<用户 id>"; the user id may itself contain colons (telegram:chat:<chat id>). */
  private split(identity: string): { platform: string; userId: string } {
    const i = identity.indexOf(":");
    if (i <= 0 || i === identity.length - 1) {
      throw new RpcError(INVALID_PARAMS, `identity 应当写成 <平台>:<用户 id>，收到 ${JSON.stringify(identity)}`);
    }
    return { platform: identity.slice(0, i), userId: identity.slice(i + 1) };
  }

  private checkPlatform(identity: string, platform: string): void {
    if (!this.deps.platforms.adapterFor(identity)) {
      throw new RpcError(APP_ERROR, `平台 ${platform} 没有启用，无法查询 ${identity}`);
    }
  }

  /**
   * UserInfo of DESIGN 6.8 `user`. Slack and Telegram ask the platform; QQ has no such interface, and any platform
   * that cannot answer falls back to the newest message of that sender in `message_log` (only name and bot flag).
   */
  async info(identity: string): Promise<WireUserInfo | null> {
    const { platform, userId } = this.split(identity);
    this.checkPlatform(identity, platform);
    if (identity === CONSOLE_OPERATOR) {
      return { identity, userId: "operator", fullName: "控制台", userName: "operator", isBot: false };
    }
    let info: ProfileInfo | null = null;
    if (platform === "slack") {
      const adapter = this.deps.platforms.slack!;
      info = this.slackInfo(await adapter.getUser(userId));
    } else if (platform === "telegram") {
      info = await this.telegramInfo(this.deps.platforms.telegram!, userId);
    }
    if (info === null) {
      info = this.historyInfo(identity, userId);
    }
    return info === null ? null : { identity, ...info };
  }

  private slackInfo(user: UserInfo | null): ProfileInfo | null {
    if (user === null) {
      return null;
    }
    return {
      userId: user.userId,
      fullName: user.fullName,
      userName: user.userName,
      isBot: user.isBot,
      ...(user.avatarUrl !== undefined ? { avatarUrl: user.avatarUrl } : {}),
      ...(user.email !== undefined ? { email: user.email } : {}),
    };
  }

  /** telegram:<user id> and telegram:chat:<chat id>; the user id is part of the identity (DESIGN 6.5). */
  private async telegramInfo(adapter: GatewayTelegramAdapter, userId: string): Promise<ProfileInfo | null> {
    const chatId = userId.startsWith("chat:") ? userId.slice("chat:".length) : userId;
    try {
      return await adapter.profileInfo(chatId);
    } catch (e) {
      if (telegramNotFound(e)) {
        return null;
      }
      throw e;
    }
  }

  /** Last name the sender used in this channel history; outbound rows carry no sender and never match. */
  private historyInfo(identity: string, userId: string): ProfileInfo | null {
    const sender = this.deps.log.lastSender(identity);
    if (sender === undefined) {
      return null;
    }
    return { userId: userId.startsWith("chat:") ? userId.slice("chat:".length) : userId, fullName: sender.name, userName: sender.name, isBot: sender.isBot };
  }

  /**
   * Avatar of DESIGN 6.8 `avatar`: downloaded by the gateway, because a Telegram file URL contains the bot token,
   * and saved into the workspace inbox like an attachment. `null` means the platform has no avatar to give.
   */
  async avatar(identity: string): Promise<WireAvatar | null> {
    const { platform, userId } = this.split(identity);
    this.checkPlatform(identity, platform);
    let data: Buffer | null;
    switch (platform) {
      case "slack":
        data = await this.slackAvatar(await this.deps.platforms.slack!.getUser(userId));
        break;
      case "telegram":
        data = await this.telegramAvatar(this.deps.platforms.telegram!, userId);
        break;
      case "qq":
        data = await this.qqAvatar(this.deps.cfg.platforms.qq!.appId, userId);
        break;
      default:
        data = null;
    }
    if (data === null) {
      return null;
    }
    const type = imageType(data);
    if (type === null) {
      throw new Error(`${identity} 的头像不是可识别的图片（${data.length} 字节）`);
    }
    const date = localDate(new Date());
    const fileName = `${safeFileName(identity)}-avatar.${type.ext}`;
    await saveInboxFile(this.deps.cfg.workspaceDir, date, fileName, data);
    return { identity, path: `inbox/${date}/${fileName}`, mimeType: type.mimeType, size: data.length };
  }

  /** The URL comes from Slack's own users.info answer; the download keeps the SSRF checks of attachments. */
  private async slackAvatar(user: UserInfo | null): Promise<Buffer | null> {
    if (user === null || user.avatarUrl === undefined) {
      return null;
    }
    return downloadAttachment(user.avatarUrl, {
      adapter: "slack",
      limit: this.deps.cfg.imageMaxBytes,
      timeoutMs: this.deps.cfg.attachmentTimeoutMs,
    });
  }

  /**
   * Telegram avatars (DESIGN 6.2.1): a user's avatar is only the photo `getUserProfilePhotos` returns, because
   * `getChat`'s photo can be a group's picture. An identity that posts as a chat (telegram:chat:<id>) is that chat,
   * and the chat photo is its own avatar. Without a photo the answer is null.
   */
  private async telegramAvatar(adapter: GatewayTelegramAdapter, userId: string): Promise<Buffer | null> {
    const isChat = userId.startsWith("chat:");
    try {
      return isChat
        ? await adapter.chatPhoto(userId.slice("chat:".length))
        : await adapter.profilePhoto(userId);
    } catch (e) {
      if (telegramNotFound(e)) {
        return null;
      }
      throw e;
    }
  }

  /** The qlogo workaround of DESIGN 6.2.2; the placeholder announced by `X-Info` counts as no avatar. */
  private async qqAvatar(appId: string, openid: string): Promise<Buffer | null> {
    try {
      return await downloadAttachment(qqAvatarUrl(appId, openid), {
        adapter: "qq",
        hosts: ["qlogo.cn"],
        limit: this.deps.cfg.imageMaxBytes,
        timeoutMs: this.deps.cfg.attachmentTimeoutMs,
        onResponse: (response) => {
          const info = response.headers["x-info"];
          if (typeof info === "string" && info.startsWith("notexist")) {
            throw new QqNoAvatar();
          }
        },
      });
    } catch (e) {
      if (e instanceof QqNoAvatar) {
        return null;
      }
      if (e instanceof NetworkError) {
        this.deps.logger.warn({ identity: openid, err: errorText(e) }, "qq avatar download failed");
      }
      throw e;
    }
  }
}
