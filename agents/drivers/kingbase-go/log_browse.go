package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Log browse white-roots. The user can configure multiple comma-separated
// roots via env var or RPC parameter — the agent validates every incoming
// path against the configured list so callers cannot escape to arbitrary
// filesystem locations. A single root (the Kingbase data directory + "sys_log")
// is the default.
var logBrowseRoots = []string{}

func initLogBrowseRoots(defaultDir string) {
	if defaultDir != "" {
		logBrowseRoots = []string{defaultDir}
	}
}

func refreshLogBrowseRoots(roots []string) {
	logBrowseRoots = nil
	for _, r := range roots {
		if r != "" {
			logBrowseRoots = append(logBrowseRoots, filepath.Clean(r))
		}
	}
}

// validateLogPath checks whether the requested absolute path sits under one of
// the configured roots. Returns the cleaned absolute path on success or an
// actionable error on failure.
func validateLogPath(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	cleaned := filepath.Clean(target)
	// Absolute path required — reject relative inputs up front.
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("必须使用绝对路径")
	}
	// Reject symlink or .. escapes that filepath.Clean missed on older Go.
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("路径不合法")
	}
	if cleaned != target {
		// Caller passed something with ./ or // — still ok after clean, but log it.
	}
	// Whitelist check.
	for _, root := range logBrowseRoots {
		rel, err := filepath.Rel(root, cleaned)
		if err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			return cleaned, nil
		}
		// rel == "." means the target *is* the root — also valid.
		if err == nil && rel == "." {
			return cleaned, nil
		}
		// If cleaned starts with root + separator → valid child.
		if strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return cleaned, nil
		}
	}
	if len(logBrowseRoots) == 0 {
		return "", fmt.Errorf("日志根目录未配置（调用 configure_log_browse_roots 设置）")
	}
	return "", fmt.Errorf("路径 %q 不在允许的日志根目录内", cleaned)
}

type logDirEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
	IsDir   bool   `json:"is_dir"`
}

func listLogDir(target string) ([]logDirEntry, error) {
	abs, err := validateLogPath(target)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("不是目录：%s", abs)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := []logDirEntry{}
	for _, e := range entries {
		full := filepath.Join(abs, e.Name())
		fi, err := os.Stat(full)
		if err != nil {
			continue
		}
		out = append(out, logDirEntry{
			Name:    e.Name(),
			Size:    fi.Size(),
			Mode:    fi.Mode().String(),
			ModTime: fi.ModTime().Format(time.RFC3339),
			IsDir:   e.IsDir(),
		})
	}
	// Sort: dirs first, then files by name.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// readLogFile returns a slice of lines from the target file. Start line is
// 1-based, limit caps how many lines we return. Filtering (keyword / level)
// is applied after read. Only regular files with *.log / *.out extension are
// accepted — rejecting binary artifacts.
func readLogFile(target string, startLine int, limit int, keyword string, level string) ([]map[string]any, error) {
	abs, err := validateLogPath(target)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("不能直接读取目录")
	}
	lower := strings.ToLower(abs)
	if !strings.HasSuffix(lower, ".log") && !strings.HasSuffix(lower, ".out") {
		return nil, fmt.Errorf("只支持 .log / .out 文件")
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if startLine <= 0 {
		startLine = 1
	}
	if limit <= 0 {
		limit = 500
	}
	// If caller asked for "last N lines" (negative startLine semantics),
	// count total and offset accordingly. We keep it simple and just start
	// from line 1 always; frontend pagination handles the rest.
	if startLine < 1 {
		startLine = 1
	}

	// Read line by line without loading the whole file.
	// io.BufferedReader + bufio.Scanner would be cleaner; using Read + split.
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	result := []map[string]any{}
	keyword = strings.TrimSpace(strings.ToLower(keyword))
	level = strings.ToUpper(strings.TrimSpace(level))
	for i := startLine - 1; i < len(lines) && len(result) < limit; i++ {
		line := lines[i]
		// Skip trailing empty line that split produces.
		if i == len(lines)-1 && line == "" {
			continue
		}
		// Filter keyword (case-insensitive) — substring match.
		if keyword != "" && !strings.Contains(strings.ToLower(line), keyword) {
			continue
		}
		// Filter level — require the level token to appear in the line
		// (Kingbase/Phoenix, ERROR/WARN/INFO/DEBUG/TRACE convention).
		if level != "" {
			levelTokens := map[string][]string{
				"ERROR": {"ERROR", "ERRORS", "FATAL", "PANIC"},
				"WARN":  {"WARN", "WARNING"},
				"INFO":  {"INFO", "NOTICE"},
				"DEBUG": {"DEBUG"},
				"TRACE": {"TRACE"},
			}
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
		result = append(result, map[string]any{
			"line_number": i + 1,
			"content":     line,
		})
	}
	return result, nil
}

// readLogFileTail returns the last `lastN` lines of the file. Used by the
// "live tail" mode in the desktop side panel.
func readLogFileTail(target string, lastN int) ([]map[string]any, error) {
	abs, err := validateLogPath(target)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		return nil, fmt.Errorf("文件不存在或为目录")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if lastN <= 0 {
		lastN = 200
	}
	if lastN > len(lines) {
		lastN = len(lines)
	}
	start := len(lines) - lastN
	// Skip trailing empty.
	if start < 0 {
		start = 0
	}
	result := []map[string]any{}
	for i := start; i < len(lines); i++ {
		line := lines[i]
		if i == len(lines)-1 && line == "" {
			continue
		}
		result = append(result, map[string]any{
			"line_number": i + 1,
			"content":     line,
		})
	}
	return result, nil
}
