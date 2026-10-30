// Package cli implements the management subcommands of DESIGN 5.11 that talk to the running daemon over admin.sock
// (contracts 7): the answer is NDJSON; lines go to stdout, the error of a failed request to stderr (by the caller),
// and the command fails.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// Client calls admin.sock.
type Client struct {
	Socket string
	Out    io.Writer
}

// Do sends the request, writes every answer line to Out and returns the error of a failed request.
func (c *Client) Do(ctx context.Context, method, path string, body any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://bearbotd"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		},
	}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接 %s（bearbotd 是否在运行？）：%w", c.Socket, err)
	}
	defer resp.Body.Close()
	r := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec struct {
				Line  *string `json:"line"`
				Done  bool    `json:"done"`
				OK    bool    `json:"ok"`
				Error string  `json:"error"`
			}
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				return fmt.Errorf("bearbotd 的回答无法解析：%q：%w", line, jerr)
			}
			if rec.Done {
				if !rec.OK {
					return errors.New(rec.Error)
				}
				return nil
			}
			if rec.Line != nil {
				if _, err := fmt.Fprintln(c.Out, *rec.Line); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			return errors.New("bearbotd 的回答没有结束记录（连接中断）")
		}
		if err != nil {
			return fmt.Errorf("读取 bearbotd 的回答失败：%w", err)
		}
	}
}

// Commands returns the management subcommands; client returns the client for the resolved --root.
func Commands(client func() (*Client, error)) []*cobra.Command {
	run := func(method, path string, body func(args []string) any) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var b any
			if body != nil {
				b = body(args)
			}
			return c.Do(cmd.Context(), method, path, b)
		}
	}
	empty := func([]string) any { return struct{}{} }

	status := &cobra.Command{
		Use:   "status",
		Short: "gateway 与各平台的连接状态、agent 状态、PID、运行时长、重启次数、当前故障、维护状态",
		Args:  cobra.NoArgs,
		RunE:  run(http.MethodGet, "/admin/v1/status", nil),
	}

	var lines int
	var follow bool
	logs := &cobra.Command{
		Use:   "logs <daemon|gateway|agent|recovery:<故障id>>",
		Short: "查看日志",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			q := url.Values{}
			q.Set("target", args[0])
			q.Set("n", strconv.Itoa(lines))
			if follow {
				q.Set("follow", "1")
			} else {
				q.Set("follow", "0")
			}
			return c.Do(cmd.Context(), http.MethodGet, "/admin/v1/logs?"+q.Encode(), nil)
		},
	}
	logs.Flags().IntVarP(&lines, "lines", "n", 100, "显示最后多少行")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "持续输出新的日志")

	var ttl string
	token := &cobra.Command{
		Use:   "token",
		Short: "生成一次性管理员令牌并打印",
		Args:  cobra.NoArgs,
		RunE:  run(http.MethodPost, "/admin/v1/admin/token", func([]string) any { return map[string]string{"ttl": ttl} }),
	}
	token.Flags().StringVar(&ttl, "ttl", "", "令牌的有效期，例如 10m（默认为 [gateway] token_ttl）")
	admin := &cobra.Command{Use: "admin", Short: "管理员管理", Args: cobra.NoArgs}
	admin.AddCommand(
		token,
		&cobra.Command{
			Use:   "list",
			Short: "管理员列表（控制台启用时包含内置的 console:operator）",
			Args:  cobra.NoArgs,
			RunE:  run(http.MethodPost, "/admin/v1/admin/list", empty),
		},
		&cobra.Command{
			Use:   "revoke <身份>",
			Short: "撤销管理员",
			Args:  cobra.ExactArgs(1),
			RunE:  run(http.MethodPost, "/admin/v1/admin/revoke", func(a []string) any { return map[string]string{"identity": a[0]} }),
		},
	)

	incident := &cobra.Command{Use: "incident", Short: "故障记录", Args: cobra.NoArgs}
	incident.AddCommand(
		&cobra.Command{Use: "list", Short: "故障列表", Args: cobra.NoArgs, RunE: run(http.MethodPost, "/admin/v1/incident/list", empty)},
		&cobra.Command{
			Use:   "show <故障id>",
			Short: "故障详情",
			Args:  cobra.ExactArgs(1),
			RunE:  run(http.MethodPost, "/admin/v1/incident/show", func(a []string) any { return map[string]string{"id": a[0]} }),
		},
	)

	var force bool
	repair := &cobra.Command{
		Use:   "repair [--force] [说明]",
		Short: "维护状态下带着说明重新运行 recovery；加 --force 时停止正在运行的 agent，建立故障并修复",
		Args:  cobra.ArbitraryArgs,
		RunE: run(http.MethodPost, "/admin/v1/repair", func(a []string) any {
			return map[string]any{"note": strings.Join(a, " "), "force": force}
		}),
	}
	repair.Flags().BoolVar(&force, "force", false, "在 running 或 draining 状态下中止当前回合、停止 agent 进程，建立故障并修复")
	// As in `/bb repair`, --force counts only before the note; option words in the note belong to it.
	repair.Flags().SetInterspersed(false)

	snapshot := &cobra.Command{Use: "snapshot", Short: "快照", Args: cobra.NoArgs}
	snapshot.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "最近 20 个快照、last-good、runningSnapshot 与回滚的默认目标",
			Args:  cobra.NoArgs,
			RunE:  run(http.MethodPost, "/admin/v1/snapshot/list", empty),
		},
		&cobra.Command{
			Use:   "diff <引用> [引用]",
			Short: "两个快照之间的 git diff --stat；只给一个引用时与它的父提交比较",
			Args:  cobra.RangeArgs(1, 2),
			RunE: run(http.MethodPost, "/admin/v1/snapshot/diff", func(a []string) any {
				body := map[string]string{"ref": a[0]}
				if len(a) == 2 {
					body["ref2"] = a[1]
				}
				return body
			}),
		},
	)

	return []*cobra.Command{
		status,
		logs,
		admin,
		{Use: "restart", Short: "重启 agent 进程（等待当前回合结束）", Args: cobra.NoArgs, RunE: run(http.MethodPost, "/admin/v1/restart", empty)},
		incident,
		repair,
		{Use: "resume", Short: "维护状态下不做改动，恢复运行", Args: cobra.NoArgs, RunE: run(http.MethodPost, "/admin/v1/resume", empty)},
		{
			Use:   "rollback [快照]",
			Short: "回滚到快照，默认为最近一次应用之前的状态",
			Args:  cobra.MaximumNArgs(1),
			RunE: run(http.MethodPost, "/admin/v1/rollback", func(a []string) any {
				if len(a) == 1 {
					return map[string]string{"snapshot": a[0]}
				}
				return struct{}{}
			}),
		},
		snapshot,
	}
}
