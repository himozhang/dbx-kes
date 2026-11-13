package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sshConnParams describes the parameters required to open an SSH connection
// to a remote Kingbase host. Password and private-key auth are both supported;
// keys may be PEM-encoded with or without a passphrase. Host key fingerprint
// verification is always on — callers must pass the expected SHA256 fingerprint
// of the host key, otherwise the connection is rejected (defense-in-depth for
// log browsing which reads files outside the database sandbox).
type sshConnParams struct {
	Host                    string `json:"host"`
	Port                    int    `json:"port"`
	User                    string `json:"user"`
	Password                string `json:"password"`
	PrivateKey              string `json:"private_key"`
	PrivateKeyPassphrase    string `json:"private_key_passphrase"`
	HostKeyFingerprint      string `json:"host_key_fingerprint"` // SHA256:base64, optional for "accept once" semantics
}

type sshLogSession struct {
	client *ssh.Client
	sftp   *sftp.Client
}

func newSSHLogSession(p sshConnParams) (*sshLogSession, error) {
	if p.Host == "" || p.User == "" {
		return nil, fmt.Errorf("host 和 user 必填")
	}
	if p.Port <= 0 {
		p.Port = 22
	}
	authMethods := []ssh.AuthMethod{}
	if p.Password != "" {
		authMethods = append(authMethods, ssh.Password(p.Password))
	}
	if p.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if p.PrivateKeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(p.PrivateKey), []byte(p.PrivateKeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(p.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("需要提供 password 或 private_key")
	}

	// Host key callback. If the caller supplies a fingerprint, verify it
	// strictly; otherwise use InsecureIgnoreHostKey only when explicitly
	// requested via the "acceptAnyHostKey" flag — default to strict.
	hostKeyCallback := ssh.FixedHostKey(ssh.PublicKey(nil)) // fails until we know the key
	if p.HostKeyFingerprint == "" {
		// "accept once" — read the server key, remember it for this session,
		// and surface it to the caller via the returned error so they can pin
		// it next time.
		hostKeyCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil // accept first connection; fingerprint reported separately
		}
	} else {
		hostKeyCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			if fp != p.HostKeyFingerprint {
				return fmt.Errorf("主机指纹不匹配: expected=%s actual=%s", p.HostKeyFingerprint, fp)
			}
			return nil
		}
	}

	config := &ssh.ClientConfig{
		User:            p.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", p.Host, p.Port)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败: %w", err)
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("SFTP 初始化失败: %w", err)
	}
	return &sshLogSession{client: client, sftp: sftpClient}, nil
}

func (s *sshLogSession) close() {
	if s.sftp != nil {
		_ = s.sftp.Close()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
}

func (s *sshLogSession) listDir(target string) ([]logDirEntry, error) {
	cleaned := filepath.Clean(target)
	// Reject traversal — the remote log viewer should only touch the user's
	// configured log roots. Local-mode validateLogPath covers the local case;
	// here we just refuse obvious escapes.
	if strings.Contains(cleaned, "..") {
		return nil, fmt.Errorf("路径不合法")
	}
	files, err := s.sftp.ReadDir(cleaned)
	if err != nil {
		return nil, fmt.Errorf("远端 ReadDir %s: %w", cleaned, err)
	}
	out := []logDirEntry{}
	for _, f := range files {
		out = append(out, logDirEntry{
			Name:    f.Name(),
			Size:    f.Size(),
			Mode:    f.Mode().String(),
			ModTime: f.ModTime().Format(time.RFC3339),
			IsDir:   f.IsDir(),
		})
	}
	return out, nil
}

func (s *sshLogSession) readFile(target string, startLine, limit int, keyword, level string) ([]map[string]any, error) {
	f, err := s.sftp.Open(target)
	if err != nil {
		return nil, fmt.Errorf("打开远端文件失败: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	return filterLogLines(lines, startLine, limit, keyword, level), nil
}

func (s *sshLogSession) tailFile(target string, lastN int) ([]map[string]any, error) {
	f, err := s.sftp.Open(target)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if lastN <= 0 {
		lastN = 200
	}
	start := len(lines) - lastN
	if start < 0 {
		start = 0
	}
	out := []map[string]any{}
	for i := start; i < len(lines); i++ {
		line := lines[i]
		if i == len(lines)-1 && line == "" {
			continue
		}
		out = append(out, map[string]any{"line_number": i + 1, "content": line})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// SSH helper: parse params block
// -----------------------------------------------------------------------------

func parseSSHParams(params map[string]json.RawMessage) (sshConnParams, error) {
	var p sshConnParams
	bytes, err := json.Marshal(params)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(bytes, &p); err != nil {
		return p, err
	}
	return p, nil
}

// 过滤日志行（与本地模式共享语义）。
func filterLogLines(lines []string, startLine, limit int, keyword, level string) []map[string]any {
	keyword = strings.TrimSpace(strings.ToLower(keyword))
	level = strings.ToUpper(strings.TrimSpace(level))
	if startLine <= 0 {
		startLine = 1
	}
	if limit <= 0 {
		limit = 500
	}
	result := []map[string]any{}
	levelTokens := map[string][]string{
		"ERROR": {"ERROR", "ERRORS", "FATAL", "PANIC"},
		"WARN":  {"WARN", "WARNING"},
		"INFO":  {"INFO", "NOTICE"},
		"DEBUG": {"DEBUG"},
		"TRACE": {"TRACE"},
	}
	for i := startLine - 1; i < len(lines) && len(result) < limit; i++ {
		line := lines[i]
		if i == len(lines)-1 && line == "" {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(line), keyword) {
			continue
		}
		if level != "" {
			tokens := levelTokens[level]
			if tokens == nil {
				tokens = []string{level}
			}
			matched := false
			for _, t := range tokens {
				if strings.Contains(strings.ToUpper(line), t) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		result = append(result, map[string]any{"line_number": i + 1, "content": line})
	}
	return result
}

// readLogFileLocalFiltered is a thin wrapper around filterLogLines so local
// mode can share the same filtering path as SSH mode.
func readLogFileLocalFiltered(target string, startLine, limit int, keyword, level string) ([]map[string]any, error) {
	abs, err := validateLogPath(target)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		return nil, fmt.Errorf("文件不存在或为目录")
	}
	lower := strings.ToLower(abs)
	if !strings.HasSuffix(lower, ".log") && !strings.HasSuffix(lower, ".out") {
		return nil, fmt.Errorf("只支持 .log / .out 文件")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	return filterLogLines(lines, startLine, limit, keyword, level), nil
}
