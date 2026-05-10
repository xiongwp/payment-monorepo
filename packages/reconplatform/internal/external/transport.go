// transport.go — 文件来源实现：local / sftp / http(s)。
//
// 不引 sftp 强依赖：sftp 用 net/exec 调系统 sftp/scp 命令，避免 go.mod 加大。
// 严格生产部署应该换成 github.com/pkg/sftp，但 dev / 多数渠道公司也接受 SCP。

package external

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// LocalTransport 从本地文件读 — dev / 单元测试 / 一次性手工导入。
type LocalTransport struct {
	BasePath string // 文件相对/绝对路径前缀
}

func (LocalTransport) Name() string         { return "local" }
func (l LocalTransport) DefaultPath() string { return l.BasePath }

func (l LocalTransport) Fetch(ctx context.Context, path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// HTTPTransport 从 HTTPS URL 拉文件 — 渠道公司提供下载地址（带 token）。
type HTTPTransport struct {
	URL          string
	Bearer       string            // optional auth header
	Headers      map[string]string // 附加 header
	Timeout      time.Duration     // 默认 30s
}

func (HTTPTransport) Name() string         { return "http" }
func (h HTTPTransport) DefaultPath() string { return h.URL }

func (h HTTPTransport) Fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	timeout := h.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	// 注意 cancel 不在 fetch return 后立刻调（要让 reader 用完），交给 caller defer rc.Close。
	req, err := http.NewRequestWithContext(cctx, "GET", url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if h.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+h.Bearer)
	}
	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("http GET %s: status %d", url, resp.StatusCode)
	}
	return &cancelReader{ReadCloser: resp.Body, cancel: cancel}, nil
}

// SFTPTransport 通过外部 sftp 客户端拉文件。生产建议换 pkg/sftp，
// 这里走 shell 出去保持依赖轻量。
//
//	echo 'get /reports/20260510/file.csv -' | sftp user@host  → 文件流到 stdout
//
// 仅支持 password / key-based ssh agent；高级 host key 校验不做（生产请加）。
type SFTPTransport struct {
	Host        string
	Port        int      // 默认 22
	User        string
	Password    string   // optional；推荐 ssh-agent
	IdentityKey string   // optional：~/.ssh/id_rsa 路径
	BasePath    string   // 同 Local 的 base
	StrictHost  bool     // dev 默认 false (skip host key check)
}

func (SFTPTransport) Name() string         { return "sftp" }
func (s SFTPTransport) DefaultPath() string { return s.BasePath }

func (s SFTPTransport) Fetch(ctx context.Context, path string) (io.ReadCloser, error) {
	port := s.Port
	if port == 0 {
		port = 22
	}
	args := []string{
		"-P", fmt.Sprintf("%d", port),
		"-o", "BatchMode=yes",
	}
	if !s.StrictHost {
		args = append(args, "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null")
	}
	if s.IdentityKey != "" {
		args = append(args, "-i", s.IdentityKey)
	}
	// sftp 自身命令格式：sftp user@host:remote_path local_dest
	// 用 - 作为 dest 让数据进 stdout
	args = append(args, fmt.Sprintf("%s@%s", s.User, s.Host))
	// 改用 echo "get path -" 管道喂 sftp（防交互）
	cmd := exec.CommandContext(ctx, "sftp", args...)
	cmd.Stdin = strings.NewReader("get " + path + " -\n" + "bye\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReader{ReadCloser: stdout, cmd: cmd}, nil
}

// cancelReader wraps a body with cancel func for HTTP timeout.
type cancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (cr *cancelReader) Close() error {
	defer cr.cancel()
	return cr.ReadCloser.Close()
}

// cmdReader wraps stdout pipe + cmd.Wait on close.
type cmdReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (cr *cmdReader) Close() error {
	cr.ReadCloser.Close()
	return cr.cmd.Wait()
}
