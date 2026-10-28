package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const goodExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.registerCommand("e2e-good", { description: "e2e", handler: async () => {} });
}
`

const typeErrorExtension = `import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  const n: number = "not a number";
  pi.registerCommand("e2e-bad", { description: String(n), handler: async () => {} });
}
`

// bridgeCounts counts the connection records of bearbot-im: gateway.sock hellos accepted and refused (gateway.log),
// bridge/ready calls and ended events connections (daemon.log).
type bridgeCounts struct {
	connected, refused, ready, eventsEnded int
}

func (b *bed) bridgeConnections() bridgeCounts {
	b.t.Helper()
	p := b.env.Paths
	return bridgeCounts{
		connected:   b.countLines(p.GatewayLog, "bearbot-im connected"),
		refused:     b.countLines(p.GatewayLog, "hello rejected: another bearbot-im connection is active"),
		ready:       b.countLines(p.DaemonLog, "msg=bridge/ready"),
		eventsEnded: b.countLines(p.DaemonLog, "bridge events connection ended"),
	}
}

type checkReport struct {
	ID       string `json:"id"`
	Passed   bool   `json:"passed"`
	Location struct {
		Passed bool `json:"passed"`
	} `json:"location"`
	Typecheck struct {
		Ran    bool `json:"ran"`
		Passed bool `json:"passed"`
		Errors []struct {
			File *string `json:"file"`
			Code string  `json:"code"`
		} `json:"errors"`
	} `json:"typecheck"`
	Trial struct {
		Ran             bool `json:"ran"`
		Passed          bool `json:"passed"`
		ExtensionErrors []struct {
			Error string `json:"error"`
		} `json:"extensionErrors"`
	} `json:"trial"`
	ReportPath string `json:"reportPath"`
}

// TestCheckAndApply: POST /v1/check with a good and a broken extension, then POST /v1/apply: snapshot apply/1, the
// restart after the (idle) turn, the applied delivery, probation, and last-good moved to apply/1 at its end.
func TestCheckAndApply(t *testing.T) {
	b := newBed(t, nil)
	b.start()
	b.waitState("running", 90*time.Second)
	// The first continuous run after the daemon started moves last-good to init.
	b.waitFor("last-good at init", 30*time.Second, func() bool {
		info, _ := b.agentStatus()
		return info.LastGood != nil && *info.LastGood == "init"
	})

	b.ext("good.ts", goodExtension)
	var rep checkReport
	// The trial process neither connects to the gateway nor calls agent.sock (DESIGN 16 检查).
	connections := b.bridgeConnections()
	if code := b.call("POST", "/v1/check", map[string]any{}, &rep); code != 200 || !rep.Passed || !rep.Typecheck.Ran || !rep.Trial.Ran || !rep.Trial.Passed {
		t.Fatalf("check of a good extension: %d %+v", code, rep)
	}
	if got := b.bridgeConnections(); got != connections {
		t.Fatalf("connections of bearbot-im changed during the check: %+v, before %+v", got, connections)
	}
	if !fileExists(rep.ReportPath) || !strings.HasPrefix(rep.ReportPath, b.env.Paths.ChecksDir) {
		t.Fatalf("report path %s", rep.ReportPath)
	}

	bad := b.ext("bad.ts", typeErrorExtension)
	rep = checkReport{}
	if code := b.call("POST", "/v1/check", map[string]any{}, &rep); code != 200 || rep.Passed || rep.Typecheck.Passed || rep.Trial.Ran {
		t.Fatalf("check of a type error: %d %+v", code, rep)
	}
	if len(rep.Typecheck.Errors) == 0 || rep.Typecheck.Errors[0].Code != "TS2322" || rep.Typecheck.Errors[0].File == nil || *rep.Typecheck.Errors[0].File != bad {
		t.Fatalf("type errors = %+v", rep.Typecheck.Errors)
	}

	// A factory that throws passes the type check and fails the trial start.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	boom := b.ext("boom.ts", factoryThrowExtension)
	rep = checkReport{}
	connections = b.bridgeConnections()
	if code := b.call("POST", "/v1/check", map[string]any{}, &rep); code != 200 || rep.Passed || !rep.Typecheck.Passed || !rep.Trial.Ran || rep.Trial.Passed {
		t.Fatalf("check of a throwing factory: %d %+v", code, rep)
	}
	if got := b.bridgeConnections(); got != connections {
		t.Fatalf("connections of bearbot-im changed during the check: %+v, before %+v", got, connections)
	}
	if err := os.Remove(boom); err != nil {
		t.Fatal(err)
	}
	bad = b.ext("bad.ts", typeErrorExtension)

	// An apply whose check fails changes nothing.
	var applied struct {
		Accepted bool         `json:"accepted"`
		Snapshot string       `json:"snapshot"`
		Report   *checkReport `json:"report"`
	}
	origin := map[string]any{"thread": "console:main", "requester": "console:operator"}
	if code := b.call("POST", "/v1/apply", map[string]any{"summary": "坏的改动", "origin": origin}, &applied); code != 200 || applied.Accepted || applied.Report == nil || applied.Report.Passed {
		t.Fatalf("apply with a failing check: %d %+v", code, applied)
	}
	if contains(b.gitTags(), "apply/1") {
		t.Fatal("apply/1 committed although the check failed")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}

	before, _ := b.agentStatus()
	applied = struct {
		Accepted bool         `json:"accepted"`
		Snapshot string       `json:"snapshot"`
		Report   *checkReport `json:"report"`
	}{}
	if code := b.call("POST", "/v1/apply", map[string]any{"summary": "添加 e2e-good 命令", "origin": origin}, &applied); code != 200 || !applied.Accepted || applied.Snapshot != "apply/1" {
		t.Fatalf("apply: %d %+v", code, applied)
	}
	// The agent drains and restarts with the same session.
	b.waitFor("restart after the apply", 60*time.Second, func() bool {
		info, code := b.agentStatus()
		return code == 200 && info.State == "running" && info.PID != nil && *info.PID != *before.PID
	})
	after, _ := b.agentStatus()
	if after.RunningSnapshot == nil || *after.RunningSnapshot != "apply/1" || after.ProbationUntil == nil || *after.SessionID != *before.SessionID || after.Restarts != 1 {
		t.Fatalf("status after the apply: %+v", after)
	}
	st := b.state()
	if st.RunningSnapshot != b.gitRev("apply/1") || st.PendingApply != nil || st.Deliver.Applied != nil || !st.LastGoodPending {
		t.Fatalf("state after the apply: %+v", st)
	}
	daemonLog := b.readFile(b.env.Paths.DaemonLog)
	if !strings.Contains(daemonLog, "applied=true") || !strings.Contains(daemonLog, "e2e-good") {
		t.Fatal("bridge/ready after the apply did not deliver applied or the new command is missing")
	}
	exits := b.logLines(b.env.Paths.AgentExitsLog)
	if len(exits) != 1 || !strings.Contains(exits[0], `"expected":true`) {
		t.Fatalf("exits.jsonl = %q", exits)
	}
	msg, err := exec.Command("git", "--git-dir="+b.env.Paths.SnapshotsGit, "log", "-1", "--format=%B", "apply/1").Output()
	if err != nil || !strings.Contains(string(msg), "添加 e2e-good 命令") || !strings.Contains(string(msg), "Bearbot-Requester: console:operator") || !strings.Contains(string(msg), "Bearbot-Thread: console:main") {
		t.Fatalf("apply/1 message %q %v", msg, err)
	}
	show, err := exec.Command("git", "--git-dir="+b.env.Paths.SnapshotsGit, "show", "--name-only", "--format=", "apply/1").Output()
	if err != nil || !strings.Contains(string(show), "workspace/.pi/extensions/good.ts") {
		t.Fatalf("apply/1 files %q %v", show, err)
	}

	// Probation ends: last-good moves to apply/1.
	b.waitFor("last-good at apply/1", 30*time.Second, func() bool {
		info, _ := b.agentStatus()
		return info.LastGood != nil && *info.LastGood == "apply/1" && info.ProbationUntil == nil
	})
	if b.state().LastGoodPending {
		t.Fatal("lastGoodPending still true")
	}
	if b.gitRev("last-good") != b.gitRev("apply/1") {
		t.Fatal("last-good tag does not point at apply/1")
	}
	out, _, code := b.cli("snapshot", "list")
	if code != 0 || !strings.Contains(out, "rollback 不指定快照时的默认目标：apply/1^") || !strings.Contains(out, "last-good：apply/1") {
		t.Fatalf("snapshot list: %s", out)
	}
	out, _, code = b.cli("snapshot", "diff", "apply/1")
	if code != 0 || !strings.Contains(out, "good.ts") {
		t.Fatalf("snapshot diff apply/1: %s", out)
	}

	// POST /v1/snapshot from the agent.
	var text struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if code := b.call("POST", "/v1/snapshot", map[string]any{"action": "list"}, &text); code != 200 || !strings.Contains(text.Text, "apply/1") {
		t.Fatalf("snapshot list: %d %+v", code, text)
	}
	for _, ref := range []string{"-x", "apply/1;rm -rf /", "HEAD~1"} {
		text = struct {
			Text  string `json:"text"`
			Error string `json:"error"`
		}{}
		if code := b.call("POST", "/v1/snapshot", map[string]any{"action": "diff", "ref": ref}, &text); code != 400 || !strings.Contains(text.Error, "无效的快照引用") {
			t.Fatalf("snapshot diff %q: %d %+v", ref, code, text)
		}
	}
	if code := b.call("POST", "/v1/snapshot", map[string]any{"action": "restore", "ref": "init", "paths": []string{"workspace/AGENTS.md"}}, &text); code != 400 || !strings.Contains(text.Error, "只能恢复重启生效路径") {
		t.Fatalf("restore of AGENTS.md: %d %+v", code, text)
	}
	// paths are relative to /bearbot.
	if code := b.call("POST", "/v1/snapshot", map[string]any{"action": "restore", "ref": "apply/1^", "paths": []string{b.env.Paths.WorkspaceDir + "/.pi/extensions/good.ts"}}, &text); code != 400 || !strings.Contains(text.Error, "相对 /bearbot 的路径") {
		t.Fatalf("restore of an absolute path: %d %+v", code, text)
	}
	var restored struct {
		Restored []string `json:"restored"`
		Text     string   `json:"text"`
	}
	if code := b.call("POST", "/v1/snapshot", map[string]any{"action": "restore", "ref": "apply/1^", "paths": []string{"workspace/.pi/extensions/good.ts"}}, &restored); code != 200 || len(restored.Restored) != 1 || restored.Restored[0] != "workspace/.pi/extensions/good.ts" {
		t.Fatalf("restore: %d %+v", code, restored)
	}
	if fileExists(b.env.Paths.WorkspaceDir + "/.pi/extensions/good.ts") {
		t.Fatal("good.ts still there after restoring apply/1^")
	}
	// The running process keeps its extension until the next apply.
	if info, _ := b.agentStatus(); info.State != "running" || *info.PID != *after.PID {
		t.Fatalf("restore restarted the agent: %+v", info)
	}
}
