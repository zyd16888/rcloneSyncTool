package server

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// parseSizeBytes parses strings like "10M", "1.5G", "1024", "8MiB".
// Suffixes are treated as binary (KiB/MiB/GiB...) to match rclone conventions.
func parseSizeBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	s = strings.ReplaceAll(s, " ", "")
	if strings.EqualFold(s, "0") {
		return 0, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return 0, errors.New("size must be >= 0")
		}
		return n, nil
	}

	orig := s
	s = strings.ToUpper(s)

	mult := int64(1)
	unit := ""
	switch {
	case strings.HasSuffix(s, "KIB"):
		unit = "KIB"
	case strings.HasSuffix(s, "MIB"):
		unit = "MIB"
	case strings.HasSuffix(s, "GIB"):
		unit = "GIB"
	case strings.HasSuffix(s, "TIB"):
		unit = "TIB"
	case strings.HasSuffix(s, "PIB"):
		unit = "PIB"
	case strings.HasSuffix(s, "EIB"):
		unit = "EIB"
	case strings.HasSuffix(s, "KB"):
		unit = "KB"
	case strings.HasSuffix(s, "MB"):
		unit = "MB"
	case strings.HasSuffix(s, "GB"):
		unit = "GB"
	case strings.HasSuffix(s, "TB"):
		unit = "TB"
	case strings.HasSuffix(s, "PB"):
		unit = "PB"
	case strings.HasSuffix(s, "EB"):
		unit = "EB"
	default:
		if len(s) > 0 {
			last := s[len(s)-1]
			if last == 'K' || last == 'M' || last == 'G' || last == 'T' || last == 'P' || last == 'E' {
				unit = string(last)
			}
		}
	}
	switch unit {
	case "":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1024
	case "M", "MB", "MIB":
		mult = 1024 * 1024
	case "G", "GB", "GIB":
		mult = 1024 * 1024 * 1024
	case "T", "TB", "TIB":
		mult = 1024 * 1024 * 1024 * 1024
	case "P", "PB", "PIB":
		mult = 1024 * 1024 * 1024 * 1024 * 1024
	case "E", "EB", "EIB":
		mult = 1024 * 1024 * 1024 * 1024 * 1024 * 1024
	default:
		return 0, errors.New("invalid size unit")
	}

	numStr := s
	if unit != "" {
		numStr = strings.TrimSuffix(numStr, unit)
	}
	if unit == "" && strings.HasSuffix(numStr, "B") {
		numStr = strings.TrimSuffix(numStr, "B")
	}
	if numStr == "" {
		return 0, errors.New("invalid size: " + orig)
	}
	f, err := strconv.ParseFloat(numStr, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("invalid size: " + orig)
	}
	if f < 0 {
		return 0, errors.New("size must be >= 0")
	}
	v := f * float64(mult)
	if v > float64(math.MaxInt64) {
		return 0, errors.New("size too large")
	}
	return int64(v + 0.5), nil
}
