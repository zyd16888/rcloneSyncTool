package store

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func WriteRcloneConfigFromDB(ctx context.Context, st *Store, path string) error {
	remotes, err := st.ListRemotes(ctx)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := bufio.NewWriter(f)
	for i, r := range remotes {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "[%s]\n", r.Name)
		fmt.Fprintf(w, "type = %s\n", r.Type)

		keys := make([]string, 0, len(r.Config))
		for k := range r.Config {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := r.Config[k]
			if strings.TrimSpace(k) == "" {
				continue
			}
			fmt.Fprintf(w, "%s = %s\n", k, v)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

