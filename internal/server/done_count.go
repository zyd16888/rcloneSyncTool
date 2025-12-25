package server

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

type doneCountCacheEntry struct {
	LogPath  string
	Offset   int64
	Carry    string
	Done     map[string]struct{}
	LastSize int64
	LastMod  time.Time
}

func (s *Server) jobDoneCount(jobID string, jobLogPath string) (int, string) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(jobLogPath) == "" {
		return 0, ""
	}
	logPath, err := safeLogPath(s.logDir, jobLogPath)
	if err != nil {
		return 0, "invalid log path"
	}
	n, err := s.doneCountFromLog(jobID, logPath)
	if err != nil {
		return 0, err.Error()
	}
	return n, ""
}

func (s *Server) doneCountFromLog(jobID string, logPath string) (int, error) {
	s.doneMu.Lock()
	defer s.doneMu.Unlock()

	ent := s.doneCache[jobID]
	if ent == nil || ent.LogPath != logPath {
		ent = &doneCountCacheEntry{
			LogPath: logPath,
			Done:    map[string]struct{}{},
		}
		s.doneCache[jobID] = ent
	}
	if ent.Done == nil {
		ent.Done = map[string]struct{}{}
	}

	info, err := os.Stat(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if info.Size() == ent.LastSize && info.ModTime().Equal(ent.LastMod) {
		return len(ent.Done), nil
	}

	// Log rotated/truncated.
	if info.Size() < ent.Offset {
		ent.Offset = 0
		ent.Carry = ""
		ent.Done = map[string]struct{}{}
	}

	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	if ent.Offset > 0 {
		if _, err := f.Seek(ent.Offset, io.SeekStart); err != nil {
			ent.Offset = 0
			ent.Carry = ""
			ent.Done = map[string]struct{}{}
			_, _ = f.Seek(0, io.SeekStart)
		}
	}

	rd := bufio.NewReader(f)
	carry := ent.Carry
	ent.Carry = ""

	for {
		line, rerr := rd.ReadString('\n')
		if rerr == nil {
			full := carry + line
			carry = ""
			if p, ok := parseTransferredPathLine(strings.TrimRight(full, "\r\n")); ok {
				ent.Done[p] = struct{}{}
			}
			continue
		}
		if errors.Is(rerr, io.EOF) {
			carry += line
			break
		}
		return len(ent.Done), rerr
	}

	ent.Carry = carry
	ent.Offset = info.Size()
	ent.LastSize = info.Size()
	ent.LastMod = info.ModTime()
	return len(ent.Done), nil
}

func parseTransferredPathLine(line string) (string, bool) {
	markers := []string{": Copied", ": Moved", ": Skipped"}
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
	// Typical: "2025/12/25 20:08:51 INFO  : path/to/file"
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

