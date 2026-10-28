package snapshot

import (
	"context"
	"fmt"
)

// Describe returns the commit time, summary, requester and thread of the snapshot ref names.
func (r *Repo) Describe(ctx context.Context, ref string) (*Entry, error) {
	commit, err := r.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	hist, err := r.history(ctx, commit)
	if err != nil {
		return nil, err
	}
	if len(hist) == 0 || hist[0].commit != commit {
		return nil, fmt.Errorf("git log 没有返回快照 %s 的提交", ref)
	}
	h := hist[0]
	return &Entry{Ref: ref, Commit: commit, Time: h.time, Summary: h.summary, Requester: h.requester, Thread: h.thread}, nil
}
