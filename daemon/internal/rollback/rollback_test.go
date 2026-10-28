package rollback

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
	"bearbot/daemon/internal/testenv"
)

// TestMain runs snapshot.HelperCommand when the snapshot repository of a test starts this test binary as its helper
// (the repository runs the running executable, as it runs bearbotd in production).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == snapshot.HelperCommand {
		if err := snapshot.RunHelper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestMerge(t *testing.T) {
	alice, bob, thread, thread2 := "telegram:1", "telegram:2", "telegram:-100", "console:main"
	r1 := Merge(nil, 1, "apply/2^", "rollback/1/before", []state.Reverted{{Ref: "apply/2", Summary: "二"}}, &alice, &thread)
	want := &state.Rollback{ID: 1, Target: "apply/2^", Saved: "rollback/1/before", Reverted: []state.Reverted{{Ref: "apply/2", Summary: "二"}}, Requesters: []string{alice}, Thread: &thread}
	if !reflect.DeepEqual(r1, want) {
		t.Fatalf("new record = %+v", r1)
	}
	r1.Notified = true
	r2 := Merge(r1, 2, "apply/1^", "rollback/2/before", []state.Reverted{{Ref: "apply/1", Summary: "一"}, {Ref: "apply/2", Summary: "二"}}, &bob, &thread2)
	if r2.ID != 2 || r2.Target != "apply/1^" || r2.Saved != "rollback/2/before" || *r2.Thread != thread2 || r2.Notified {
		t.Fatalf("merged = %+v", r2)
	}
	if !reflect.DeepEqual(r2.Requesters, []string{alice, bob}) {
		t.Fatalf("requesters = %v", r2.Requesters)
	}
	if !reflect.DeepEqual(r2.Reverted, []state.Reverted{{Ref: "apply/2", Summary: "二"}, {Ref: "apply/1", Summary: "一"}}) {
		t.Fatalf("reverted = %v", r2.Reverted)
	}
	// From the command line: no requester, thread null; the earlier requesters stay.
	r3 := Merge(r2, 3, "init", "rollback/3/before", nil, nil, nil)
	if r3.Thread != nil || !reflect.DeepEqual(r3.Requesters, []string{alice, bob}) || len(r3.Reverted) != 2 {
		t.Fatalf("merged from the command line = %+v", r3)
	}
	cli := Merge(nil, 1, "init", "rollback/1/before", nil, nil, nil)
	if cli.Requesters != nil || cli.Reverted == nil || len(cli.Reverted) != 0 {
		t.Fatalf("command line record = %+v", cli)
	}
}

func TestTarget(t *testing.T) {
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
	repo, err := snapshot.Open(e.Paths, u)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Target(ctx, repo, ""); err == nil || !strings.Contains(err.Error(), "还没有任何应用记录") {
		t.Fatalf("default target without applies: %v", err)
	}
	for _, bad := range []string{"-x", "apply/1;rm", "HEAD", "apply/x"} {
		if _, err := Target(ctx, repo, bad); err == nil || !strings.Contains(err.Error(), "无效的快照引用") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := Target(ctx, repo, "apply/7"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing snapshot: %v", err)
	}
	e.WriteFile(t, "/bearbot/workspace/.pi/extensions/a.ts", "export default function () {}\n")
	if _, err := repo.Commit(ctx, snapshot.ApplyTag(1), snapshot.Message{Summary: "一"}); err != nil {
		t.Fatal(err)
	}
	if got, err := Target(ctx, repo, ""); err != nil || got != "apply/1^" {
		t.Fatalf("default target = %q %v", got, err)
	}
	if got, err := Target(ctx, repo, "init"); err != nil || got != "init" {
		t.Fatalf("init = %q %v", got, err)
	}
}
