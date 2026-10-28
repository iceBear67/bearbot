import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import { repoRoot } from "./pi-rpc.ts";

interface CopyInstruction {
	line: number;
	from: string | null;
	sources: string[];
	destination: string;
}

function copyInstructions(): CopyInstruction[] {
	const lines = readFileSync(join(repoRoot, "Dockerfile"), "utf8").split("\n");
	const copies: CopyInstruction[] = [];
	lines.forEach((text, index) => {
		const words = text.trim().split(/\s+/);
		if (words[0] !== "COPY") {
			return;
		}
		const flags = words.slice(1).filter((word) => word.startsWith("--"));
		const operands = words.slice(1).filter((word) => !word.startsWith("--"));
		const from = flags.find((flag) => flag.startsWith("--from="));
		copies.push({
			line: index + 1,
			from: from === undefined ? null : from.slice("--from=".length),
			sources: operands.slice(0, -1),
			destination: operands[operands.length - 1],
		});
	});
	return copies;
}

describe("recovery profile 模板", () => {
	test("recovery-profile/settings.json 与 DESIGN 10.1 相同", () => {
		const settings: unknown = JSON.parse(readFileSync(join(repoRoot, "recovery-profile", "settings.json"), "utf8"));
		expect(settings).toEqual({
			quietStartup: true,
			defaultProjectTrust: "never",
			defaultProvider: "anthropic",
			defaultModel: "claude-opus-5-5",
		});
	});
});

describe("Dockerfile", () => {
	test("从构建上下文复制的每个路径都存在于仓库中", () => {
		const missing = copyInstructions()
			.filter((copy) => copy.from === null)
			.flatMap((copy) => copy.sources.map((source) => ({ line: copy.line, source })))
			.filter(({ source }) => !existsSync(join(repoRoot, source)));
		expect(missing).toEqual([]);
	});

	test("node-build 阶段复制出的源码路径来自仓库，其余由 npm ci 与 gateway 构建产生", () => {
		const produced = new Set(["/src/node_modules", "/src/gateway/dist"]);
		const fromNodeBuild = copyInstructions().filter((copy) => copy.from === "node-build");
		expect(fromNodeBuild.flatMap((copy) => copy.sources).sort()).toEqual(
			["/src/gateway/dist", "/src/gateway/drizzle", "/src/gateway/package.json", "/src/node_modules"].sort(),
		);
		const missing = fromNodeBuild
			.flatMap((copy) => copy.sources)
			.filter((source) => !produced.has(source))
			.filter((source) => !existsSync(join(repoRoot, source.slice("/src/".length))));
		expect(missing).toEqual([]);
	});

	test(".dockerignore 把 .git、宿主机上的 node_modules、构建产物与日志排除在构建上下文之外", () => {
		const patterns = readFileSync(join(repoRoot, ".dockerignore"), "utf8")
			.split("\n")
			.map((line) => line.trim())
			.filter((line) => line !== "" && !line.startsWith("#"));
		expect(patterns).toEqual(expect.arrayContaining([".git", "**/node_modules", "gateway/dist", "daemon/bin", "**/*.log"]));
	});

	test("镜像内的 bearbot 组件放在 contracts 1.1 的位置", () => {
		const destinations = Object.fromEntries(
			copyInstructions()
				.filter((copy) => copy.from === null && copy.sources.length === 1)
				.map((copy) => [copy.sources[0], copy.destination]),
		);
		expect(destinations["extensions/"]).toBe("/opt/bearbot/extensions/");
		expect(destinations["sdk/"]).toBe("/opt/bearbot/sdk/");
		expect(destinations["skills/"]).toBe("/opt/bearbot/skills/");
		expect(destinations["recovery-profile/"]).toBe("/opt/bearbot/recovery-profile/");
	});
});
