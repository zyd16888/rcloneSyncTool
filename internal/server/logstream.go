package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"115togd/internal/store"
)

func (s *Server) apiJobLogStream(c *gin.Context) {
	ctx := c.Request.Context()
	jobID := strings.TrimSpace(c.Query("id"))
	if jobID == "" {
		c.String(http.StatusBadRequest, "missing id")
		return
	}
	job, ok, err := s.st.GetJob(ctx, jobID)
	if err != nil || !ok {
		c.Status(http.StatusNotFound)
		return
	}
	logPath, err := safeLogPath(s.logDir, job.LogPath)
	if err != nil {
		c.String(http.StatusForbidden, "invalid log path")
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.String(http.StatusInternalServerError, "no flusher")
		return
	}

	if err := writeSSE(c.Writer, "init", ""); err != nil {
		return
	}
	flusher.Flush()

	// Wait for file creation if rclone hasn't started writing yet.
	var f *os.File
	deadline := time.Now().Add(8 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ff, err := os.Open(logPath)
		if err == nil {
			f = ff
			break
		}
		if time.Now().After(deadline) {
			_ = writeSSE(c.Writer, "log", fmt.Sprintf("日志文件尚未生成：%s", filepath.Base(logPath)))
			flusher.Flush()
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	defer f.Close()

	// Send last N lines first.
	if text, err := tailLastLines(f, 200, 1<<20); err == nil && strings.TrimSpace(text) != "" {
		_ = writeSSE(c.Writer, "log", text)
		flusher.Flush()
	}

	// Follow file.
	offset, _ := f.Seek(0, io.SeekEnd)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			info, err := f.Stat()
			if err != nil {
				return
			}
			if offset > info.Size() {
				offset = 0
				_, _ = f.Seek(0, io.SeekStart)
			}
			if offset == info.Size() {
				// If job ended and no more output, stop soon.
				if jobEnded(job) {
					_ = writeSSE(c.Writer, "done", "")
					flusher.Flush()
					return
				}
				continue
			}
			buf := make([]byte, info.Size()-offset)
			n, _ := f.ReadAt(buf, offset)
			if n <= 0 {
				continue
			}
			offset += int64(n)
			_ = writeSSE(c.Writer, "log", string(buf[:n]))
			flusher.Flush()
		}
	}
}

func jobEnded(j store.Job) bool { return j.Status == "done" || j.Status == "failed" }

func safeLogPath(logDir, jobLogPath string) (string, error) {
	base, err := filepath.Abs(logDir)
	if err != nil {
		return "", err
	}
	p := jobLogPath
	if p == "" {
		return "", errors.New("empty log path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return "", errors.New("outside log dir")
	}
	return abs, nil
}

func tailLastLines(f *os.File, lines int, maxBytes int64) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := string(b)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.Split(s, "\n")
	if len(parts) <= lines {
		return s, nil
	}
	return strings.Join(parts[len(parts)-lines:], "\n"), nil
}

func writeSSE(w io.Writer, event, data string) error {
	bw := bufio.NewWriter(w)
	if event != "" {
		if _, err := bw.WriteString("event: " + event + "\n"); err != nil {
			return err
		}
	}
	data = strings.ReplaceAll(data, "\r\n", "\n")
	for _, line := range strings.Split(data, "\n") {
		if _, err := bw.WriteString("data: " + line + "\n"); err != nil {
			return err
		}
	}
	if _, err := bw.WriteString("\n"); err != nil {
		return err
	}
	return bw.Flush()
}

