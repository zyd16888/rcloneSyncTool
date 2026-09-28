package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
)

func logHadNothingToTransfer(logPath string) bool {
	f, err := os.Open(logPath)
	if err != nil {
		return false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "There was nothing to transfer") ||
			strings.Contains(line, "There was nothing to copy") ||
			strings.Contains(line, "There was nothing to move") {
			return true
		}
	}
	return false
}

func parseTransferredPathLine(line string) (string, bool) {
	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		var entry struct {
			Msg    string `json:"msg"`
			Object string `json:"object"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Object != "" && (strings.HasPrefix(entry.Msg, "Copied") || strings.HasPrefix(entry.Msg, "Moved")) {
			return entry.Object, true
		}
		return "", false
	}
	markers := []string{": Copied", ": Moved"}
	idx := -1
	for _, m := range markers {
		if j := strings.LastIndex(line, m); j > idx {
			idx = j
		}
	}
	if idx <= 0 {
		return "", false
	}
	head := strings.TrimSpace(line[:idx])
	// Typical: "2025/12/25 14:45:20 INFO  : path/to/file"
	if j := strings.LastIndex(head, " : "); j >= 0 {
		head = head[j+3:]
	} else if j := strings.LastIndex(head, ": "); j >= 0 {
		head = head[j+2:]
	}
	p := strings.TrimSpace(head)
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return "", false
	}
	return p, true
}

func TransferredPathLine(line string) (string, bool) { return parseTransferredPathLine(line) }

func statsFromLog(logPath string, offset int64) (rcStats, bool) {
	f, err := os.Open(logPath)
	if err != nil {
		return rcStats{}, false
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return rcStats{}, false
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var last rcStats
	found := false
	for scanner.Scan() {
		var entry struct {
			Stats map[string]any `json:"stats"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Stats != nil {
			last = rcStats{Bytes: toInt64(entry.Stats["bytes"]), Speed: toFloat64(entry.Stats["speed"]), Transfers: int(toInt64(entry.Stats["transfers"])), Errors: int(toInt64(entry.Stats["errors"]))}
			found = true
		}
	}
	return last, found
}

func transferredPathsFromLog(logPath string) (map[string]struct{}, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	done := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	// Allow long lines (some backends print long messages).
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		p, ok := parseTransferredPathLine(sc.Text())
		if !ok {
			continue
		}
		done[p] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return done, nil
}
