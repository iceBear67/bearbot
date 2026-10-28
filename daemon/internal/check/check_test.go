package check

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/testenv"
)

type fixture struct {
	env     *testenv.Env
	checker *Checker
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	e := testenv.New(t)
	u := e.Users(t)
	for _, d := range e.Paths.Dirs() {
		if d.Path == e.Paths.RunDir {
			continue
		}
		if _, err := layout.EnsureDir(d, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(e.Paths.AgentSettingsFile, []byte(layout.DefaultAgentSettings), 0o664); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(e.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: e, checker: &Checker{Paths: e.Paths, Users: u, Config: cfg, Node: node}}
}

func (f *fixture) ext(t *testing.T, name, content string) string {
	t.Helper()
	return f.env.WriteFile(t, "/bearbot/workspace/.pi/extensions/"+name, content)
}

func (f *fixture) run(t *testing.T) *Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep, err := f.checker.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rep.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored Report
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ID != rep.ID || stored.Passed != rep.Passed || stored.ReportPath != rep.ReportPath {
		t.Fatalf("report.json = %s", data)
	}
	if !regexp.MustCompile(`^chk-[0-9]{14}-[0-9a-f]{4}$`).MatchString(rep.ID) {
		t.Fatalf("id = %s", rep.ID)
	}
	if rep.ReportPath != filepath.Join(f.env.Paths.ChecksDir, rep.ID, "report.json") {
		t.Fatalf("report path = %s", rep.ReportPath)
	}
	return rep
}

// requireSDK fails when the SDK of another component is missing: tsconfig.json lists it in "files".
func (f *fixture) requireSDK(t *testing.T) {
	t.Helper()
	f.env.RequireRepoFile(t, "sdk/index.ts")
}

// requireBearbotIM fails when bearbot-im (another component) is missing: the trial loads it with -e.
func (f *fixture) requireBearbotIM(t *testing.T) {
	t.Helper()
	f.requireSDK(t)
	f.env.RequireRepoFile(t, "extensions/bearbot-im/index.ts")
	f.env.RequireRepoFile(t, "extensions/bearbot-im/package.json")
}

func TestLocation(t *testing.T) {
	f := newFixture(t)
	loc, err := CheckLocation(f.env.Paths)
	if err != nil || !loc.Passed || len(loc.Violations) != 0 || loc.Message == "" {
		t.Fatalf("clean profile = %+v, %v", loc, err)
	}
	agent := f.env.Paths.AgentDir
	f.env.WriteFile(t, "/bearbot/profile/agent/extensions/x.ts", "export default function () {}\n")
	f.env.WriteFile(t, "/bearbot/profile/agent/skills/s/SKILL.md", "skill\n")
	if err := os.MkdirAll(filepath.Join(agent, "prompts"), 0o775); err != nil {
		t.Fatal(err)
	}
	f.env.WriteFile(t, "/bearbot/profile/agent/AGENTS.md", "memory\n")
	f.env.WriteFile(t, "/bearbot/profile/agent/CLAUDE.md", "")
	f.env.WriteFile(t, "/bearbot/profile/agent/mcp.json", "{}\n")
	f.env.WriteFile(t, "/bearbot/profile/agent/APPEND_SYSTEM.md", "x\n")
	if err := os.Symlink("/nonexistent", filepath.Join(agent, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	loc, err = CheckLocation(f.env.Paths)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		agent + "/extensions/", agent + "/skills/", agent + "/mcp.json", agent + "/AGENTS.md", agent + "/CLAUDE.md",
		agent + "/SYSTEM.md", agent + "/APPEND_SYSTEM.md",
	}
	if loc.Passed || strings.Join(loc.Violations, "|") != strings.Join(want, "|") {
		t.Fatalf("violations = %q", loc.Violations)
	}
	for _, w := range []string{"位置检查失败", ".pi/extensions/", ".pi/mcp.json", "AGENTS.md"} {
		if !strings.Contains(loc.Message, w) {
			t.Errorf("message %q does not contain %q", loc.Message, w)
		}
	}

	rep := f.run(t)
	if rep.Passed || rep.Location.Passed || rep.Typecheck.Ran || rep.Trial.Ran {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Typecheck.Errors == nil || rep.Trial.ExtensionErrors == nil || rep.Trial.Warnings == nil {
		t.Fatal("empty lists must be [] in the report")
	}
}

func TestTSConfig(t *testing.T) {
	f := newFixture(t)
	aliases, err := Aliases(f.env.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 22 {
		t.Fatalf("%d aliases", len(aliases))
	}
	if aliases["@earendil-works/pi-coding-agent"] != filepath.Join(f.env.Paths.PiPackage, "dist/index.d.ts") {
		t.Errorf("pi alias = %s", aliases["@earendil-works/pi-coding-agent"])
	}
	// The repository install hoists typebox next to the pi package (a global install nests it).
	if aliases["typebox/value"] != filepath.Join(f.env.Repo, "node_modules/typebox/build/value/index.d.mts") {
		t.Errorf("typebox/value = %s", aliases["typebox/value"])
	}
	if aliases["@mariozechner/pi-ai"] != filepath.Join(f.env.Repo, "node_modules/@earendil-works/pi-ai/dist/compat.d.ts") || aliases["@sinclair/typebox"] != aliases["typebox"] {
		t.Errorf("aliases = %v", aliases)
	}
	data, err := TSConfig(f.env.Paths)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CompilerOptions map[string]any `json:"compilerOptions"`
		Files           []string       `json:"files"`
		Include         []string       `json:"include"`
		Exclude         []string       `json:"exclude"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	ws := f.env.Paths.WorkspaceDir
	if cfg.Files[0] != f.env.Paths.SDKFile || cfg.Include[0] != ws+"/.pi/extensions/**/*" || cfg.Exclude[0] != ws+"/.pi/extensions/**/node_modules" {
		t.Fatalf("tsconfig = %s", data)
	}
	co := cfg.CompilerOptions
	if co["module"] != "preserve" || co["moduleResolution"] != "bundler" || co["target"] != "es2024" || co["noEmit"] != true || co["strict"] != true || co["skipLibCheck"] != true || co["allowImportingTsExtensions"] != true {
		t.Fatalf("compilerOptions = %v", co)
	}
	if _, ok := co["baseUrl"]; ok {
		t.Fatal("baseUrl must not be set")
	}
}

const goodExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { Value } from "typebox/value";
import type { AgentMessage } from "@earendil-works/pi-agent-core";
import type { InboundMessage } from "%s";
import { helper } from "./helper.ts";

export default function (pi: ExtensionAPI) {
  const keep: AgentMessage[] = [];
  const seen: InboundMessage[] = [];
  const timer: NodeJS.Timeout | undefined = undefined;
  pi.registerTool({
    name: "good_echo",
    label: "Echo",
    description: "echo the text",
    parameters: Type.Object({ text: Type.String() }),
    async execute(_id, params) {
      return { content: [{ type: "text", text: helper(params.text) + Value.Check(Type.String(), params.text) }], details: { keep, seen, timer } };
    },
  });
}
`

func (f *fixture) writeGood(t *testing.T) {
	t.Helper()
	f.ext(t, "good/index.ts", strings.Replace(goodExtension, "%s", f.env.Paths.SDKFile, 1))
	f.ext(t, "good/helper.ts", "export function helper(s: string): string { return s.toUpperCase(); }\n")
}

func TestTypecheckPass(t *testing.T) {
	f := newFixture(t)
	f.requireSDK(t)
	f.writeGood(t)
	// A dependency inside the extension directory is not checked itself.
	f.ext(t, "good/node_modules/broken/index.ts", "const x: number = 'no';\n")
	dir := t.TempDir()
	tc, err := f.checker.Typecheck(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !tc.Ran || !tc.Passed || len(tc.Errors) != 0 {
		t.Fatalf("typecheck = %+v", tc)
	}
	if _, err := os.Stat(filepath.Join(dir, "tsconfig.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tsc.out")); err != nil {
		t.Fatal(err)
	}
}

// TestInterpreterNotFromChildPATH: a `node` that bearbot put into the profile's bin directory (the first entry of the
// child PATH) is not used for tsc and the trial; they run with the node the daemon found in its own PATH.
func TestInterpreterNotFromChildPATH(t *testing.T) {
	f := newFixture(t)
	f.requireBearbotIM(t)
	f.writeGood(t)
	marker := filepath.Join(f.env.Root, "profile-node-ran")
	f.env.WriteFile(t, "/bearbot/profile/bin/node", "#!/bin/sh\ntouch "+marker+"\nexit 1\n")
	if err := os.Chmod(filepath.Join(f.env.Paths.ProfileBin, "node"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := f.run(t)
	if !rep.Passed || !rep.Typecheck.Passed || !rep.Trial.Passed {
		t.Fatalf("report = %+v", rep)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the node in %s ran: %v", f.env.Paths.ProfileBin, err)
	}
}

func TestTypecheckEmptyWorkspace(t *testing.T) {
	f := newFixture(t)
	f.requireSDK(t)
	tc, err := f.checker.Typecheck(context.Background(), t.TempDir())
	if err != nil || !tc.Passed {
		t.Fatalf("typecheck without extensions = %+v, %v", tc, err)
	}
}

func TestTypecheckErrors(t *testing.T) {
	f := newFixture(t)
	f.requireSDK(t)
	f.ext(t, "bad.ts", `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
export default function (pi: ExtensionAPI) {
  const n: number = "not a number";
  pi.on("no_such_event", () => {});
}
`)
	f.ext(t, "missing/index.ts", "import { x } from \"not-installed-pkg\";\nexport default function () { return x; }\n")
	rep := f.run(t)
	if rep.Passed || !rep.Location.Passed || !rep.Typecheck.Ran || rep.Typecheck.Passed || rep.Trial.Ran {
		t.Fatalf("report = %+v", rep)
	}
	errs := rep.Typecheck.Errors
	if len(errs) != 3 {
		t.Fatalf("errors = %+v\noutput:\n%s", errs, rep.Typecheck.Output)
	}
	bad := filepath.Join(f.env.Paths.WorkspaceDir, ".pi/extensions/bad.ts")
	e0 := errs[0]
	if e0.File == nil || *e0.File != bad || *e0.Line != 3 || *e0.Column != 9 || e0.Code != "TS2322" {
		t.Fatalf("first error = %+v (%s)", e0, *e0.File)
	}
	if errs[1].Code != "TS2769" || !strings.Contains(errs[1].Message, "\n") {
		t.Fatalf("overload error without its continuation lines: %+v", errs[1])
	}
	if errs[2].Code != "TS2307" || !strings.HasSuffix(*errs[2].File, "/.pi/extensions/missing/index.ts") {
		t.Fatalf("third error = %+v", errs[2])
	}
	if !strings.Contains(rep.Typecheck.Output, "error TS2322") {
		t.Fatalf("output = %s", rep.Typecheck.Output)
	}
}

func TestTypecheckSyntaxError(t *testing.T) {
	f := newFixture(t)
	f.requireSDK(t)
	f.ext(t, "syntax.ts", "export default function (pi: any) {\n  const x = {a: };\n}\n")
	tc, err := f.checker.Typecheck(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if tc.Passed || len(tc.Errors) == 0 || !strings.HasPrefix(tc.Errors[0].Code, "TS1") {
		t.Fatalf("typecheck = %+v", tc)
	}
}

func TestParseDiagnostics(t *testing.T) {
	out := "error TS18003: No inputs were found in config file.\n" +
		"/abs/x.ts(1,2): error TS2322: Type 'number' is not assignable to type 'string'.\n" +
		".pi/extensions/a.ts(10,5): error TS2769: No overload matches this call.\n" +
		"  The last overload gave the following error.\n" +
		"    Argument of type 'x' is not assignable.\n"
	errs := ParseDiagnostics(out, "/bearbot/workspace")
	if len(errs) != 3 || errs[0].File != nil || errs[0].Code != "TS18003" || *errs[1].File != "/abs/x.ts" || *errs[2].File != "/bearbot/workspace/.pi/extensions/a.ts" || *errs[2].Line != 10 {
		t.Fatalf("errors = %+v", errs)
	}
	if errs[2].Message != "No overload matches this call.\nThe last overload gave the following error.\nArgument of type 'x' is not assignable." {
		t.Fatalf("message = %q", errs[2].Message)
	}
}

func TestTrialPassWithMCPWarning(t *testing.T) {
	f := newFixture(t)
	f.requireBearbotIM(t)
	f.writeGood(t)
	f.env.WriteFile(t, "/bearbot/workspace/.pi/mcp.json", `{"mcpServers": {"broken": {"command": "/nonexistent/mcp-server-binary", "args": []}}}`+"\n")
	rep := f.run(t)
	if !rep.Passed || !rep.Trial.Ran || !rep.Trial.Passed || rep.Trial.TimedOut || len(rep.Trial.ExtensionErrors) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Trial.Warnings) != 1 || !strings.HasPrefix(rep.Trial.Warnings[0], MCPWarningPrefix) || !strings.Contains(rep.Trial.Warnings[0], "broken") {
		t.Fatalf("warnings = %q", rep.Trial.Warnings)
	}
	if rep.Trial.ExitCode == nil || *rep.Trial.ExitCode != 0 {
		t.Fatalf("exit = %v %v", rep.Trial.ExitCode, rep.Trial.Signal)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(rep.ReportPath), "trial-stderr.log")); err != nil {
		t.Fatal(err)
	}
}

func requireTrialFailure(t *testing.T, rep *Report, stderrPart string) {
	t.Helper()
	if rep.Passed || !rep.Typecheck.Passed || !rep.Trial.Ran || rep.Trial.Passed {
		t.Fatalf("report = %+v", rep)
	}
	if stderrPart != "" && !strings.Contains(rep.Trial.Stderr, stderrPart) {
		t.Fatalf("stderr does not contain %q:\n%s", stderrPart, rep.Trial.Stderr)
	}
}

func TestTrialFactoryThrows(t *testing.T) {
	f := newFixture(t)
	f.requireBearbotIM(t)
	f.ext(t, "throws.ts", "export default function (): void {\n  throw new Error(\"factory boom\");\n}\n")
	rep := f.run(t)
	requireTrialFailure(t, rep, "factory boom")
	if rep.Trial.ExitCode == nil || *rep.Trial.ExitCode != 1 || rep.Trial.TimedOut {
		t.Fatalf("trial = %+v", rep.Trial)
	}
	if !strings.Contains(rep.Trial.Stderr, "Failed to load extension") {
		t.Fatalf("stderr = %s", rep.Trial.Stderr)
	}
}

func TestTrialDuplicateTool(t *testing.T) {
	f := newFixture(t)
	f.requireBearbotIM(t)
	tool := `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "dup_tool",
    label: "Dup",
    description: "dup",
    parameters: Type.Object({}),
    async execute() {
      return { content: [{ type: "text", text: "x" }], details: {} };
    },
  });
}
`
	f.ext(t, "tool-a.ts", tool)
	f.ext(t, "tool-b.ts", tool)
	rep := f.run(t)
	requireTrialFailure(t, rep, "dup_tool")
	if !strings.Contains(rep.Trial.Stderr, "conflicts") {
		t.Fatalf("stderr = %s", rep.Trial.Stderr)
	}
}

func TestTrialSyntaxError(t *testing.T) {
	// A .js extension is not type checked; the trial start finds its syntax error.
	f := newFixture(t)
	f.requireBearbotIM(t)
	f.ext(t, "broken.js", "export default function (pi) {\n  const x = { a: };\n}\n")
	rep := f.run(t)
	requireTrialFailure(t, rep, "Failed to load extension")
	if !strings.Contains(rep.Trial.Stderr, "broken.js") {
		t.Fatalf("stderr = %s", rep.Trial.Stderr)
	}
}

func TestTrialExtensionError(t *testing.T) {
	f := newFixture(t)
	f.requireBearbotIM(t)
	f.ext(t, "start-throws.ts", `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
export default function (pi: ExtensionAPI) {
  pi.on("session_start", async () => {
    throw new Error("session_start boom");
  });
}
`)
	rep := f.run(t)
	requireTrialFailure(t, rep, "")
	errs := rep.Trial.ExtensionErrors
	if len(errs) != 1 || errs[0].Event != "session_start" || errs[0].Error != "session_start boom" || !strings.HasSuffix(errs[0].ExtensionPath, "start-throws.ts") {
		t.Fatalf("extension errors = %+v", errs)
	}
}

func TestTrialCrashInWindow(t *testing.T) {
	// An exception in a timer ends pi after the get_state answer, inside the warning window.
	f := newFixture(t)
	f.requireBearbotIM(t)
	f.ext(t, "timer.ts", `export default function (): void {
  setTimeout(() => {
    throw new Error("timer boom");
  }, 500);
}
`)
	rep := f.run(t)
	requireTrialFailure(t, rep, "timer boom")
	if rep.Trial.TimedOut || rep.Trial.ExitCode == nil || *rep.Trial.ExitCode == 0 {
		t.Fatalf("trial = %+v", rep.Trial)
	}
}
