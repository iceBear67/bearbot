// contracts 9.4: Telegram parts must satisfy TelegramFormatConverter().fromMarkdown(part).length <= 4096.
import { TelegramFormatConverter } from "@chat-adapter/telegram";
import { expect, test } from "vitest";
import { SLACK_MARKDOWN_LIMIT, SLACK_TEXT_LIMIT, TELEGRAM_TEXT_LIMIT, splitText } from "../src/outbound.ts";

const converter = new TelegramFormatConverter();
const measure = (s: string) => converter.fromMarkdown(s).length;

test("short text is one part", () => {
  expect(splitText("hello **world**", TELEGRAM_TEXT_LIMIT, measure, true)).toEqual(["hello **world**"]);
});

test("long markdown is cut at line ends and every part fits after MarkdownV2 escaping", () => {
  const lines = Array.from({ length: 900 }, (_, i) => `第 ${i} 行：a.b-c (d) [e] ${"x".repeat(i % 40)}.`);
  const text = lines.join("\n");
  expect(measure(text)).toBeGreaterThan(TELEGRAM_TEXT_LIMIT);
  const parts = splitText(text, TELEGRAM_TEXT_LIMIT, measure, true);
  expect(parts.length).toBeGreaterThan(1);
  for (const p of parts) {
    expect(measure(p)).toBeLessThanOrEqual(TELEGRAM_TEXT_LIMIT);
  }
  expect(parts.join("\n")).toBe(text);
});

test("a code block cut between parts is closed and reopened", () => {
  const code = Array.from({ length: 400 }, (_, i) => `const v${i} = ${i}; // ${"*".repeat(10)}`).join("\n");
  const text = `前言\n\n\`\`\`ts\n${code}\n\`\`\`\n\n结尾`;
  const parts = splitText(text, TELEGRAM_TEXT_LIMIT, measure, true);
  expect(parts.length).toBeGreaterThan(1);
  for (const p of parts) {
    expect(measure(p)).toBeLessThanOrEqual(TELEGRAM_TEXT_LIMIT);
    const fences = p.split("\n").filter((l) => l.startsWith("```")).length;
    expect(fences % 2).toBe(0);
  }
  expect(parts[1]!.startsWith("```ts\n")).toBe(true);
  expect(parts.at(-1)!.endsWith("结尾")).toBe(true);
});

test("a single line longer than the limit is cut without losing characters", () => {
  const line = "长".repeat(10_000) + "😀".repeat(100);
  const parts = splitText(line, TELEGRAM_TEXT_LIMIT, (s) => s.length, false);
  expect(parts.length).toBe(3);
  for (const p of parts) expect(p.length).toBeLessThanOrEqual(TELEGRAM_TEXT_LIMIT);
  expect(parts.join("")).toBe(line);
});

test("Slack markdown is cut at markdown_text's 12000 characters and plain text at text's 40000", () => {
  // Japanese text is one character per code point, so the source length is what Slack counts.
  const markdown = Array.from({ length: 400 }, (_, i) => `第 ${i} 行：${"あ".repeat(40)}`).join("\n");
  expect(markdown.length).toBeGreaterThan(SLACK_MARKDOWN_LIMIT);
  const markdownParts = splitText(markdown, SLACK_MARKDOWN_LIMIT, (s) => s.length, true);
  expect(markdownParts.length).toBeGreaterThan(1);
  for (const p of markdownParts) expect(p.length).toBeLessThanOrEqual(SLACK_MARKDOWN_LIMIT);
  expect(markdownParts.join("\n")).toBe(markdown);

  // The same text as plain text is one message.
  expect(splitText(markdown, SLACK_TEXT_LIMIT, (s) => s.length, false)).toEqual([markdown]);
});
