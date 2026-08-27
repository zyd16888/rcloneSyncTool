package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"115togd/internal/store"
)

// runAPIToken manages /api/v1 credentials from the terminal. Minting happens
// here rather than in the Web UI so the plaintext is shown once, on an
// operator's own console, and never rendered into a browser page or its cache.
func runAPIToken(args []string) {
	if len(args) == 0 {
		apiTokenUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("apitoken "+sub, flag.ExitOnError)
	dataDir := fs.String("data", "./data", "Data directory")
	name := fs.String("name", "", "Token name (create)")
	id := fs.String("id", "", "Token id (enable/disable/delete)")
	expiresDays := fs.Int("expires-days", 0, "Expire after N days (create; 0 = never)")
	_ = fs.Parse(args[1:])

	st := openStoreForCLI(*dataDir)
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "create":
		expiresAt := time.Time{}
		if *expiresDays > 0 {
			expiresAt = time.Now().AddDate(0, 0, *expiresDays)
		}
		token, plaintext, err := st.CreateAPIToken(ctx, *name, expiresAt)
		if err != nil {
			exitErr("create token", err)
		}
		fmt.Printf("OK: token created\n  id:    %s\n  name:  %s\n  token: %s\n", token.ID, token.Name, plaintext)
		fmt.Println("\n请立即保存该 token，它不会再次显示。")
	case "list":
		tokens, err := st.ListAPITokens(ctx)
		if err != nil {
			exitErr("list tokens", err)
		}
		if len(tokens) == 0 {
			fmt.Println("(no api tokens)")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tENABLED\tCREATED\tEXPIRES\tLAST USED")
		for _, t := range tokens {
			fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\t%s\n",
				t.ID, t.Name, t.Enabled,
				formatUnix(t.CreatedAt), formatUnix(t.ExpiresAt), formatUnix(t.LastUsedAt))
		}
		_ = w.Flush()
	case "enable", "disable":
		if *id == "" {
			exitErr("token id", fmt.Errorf("-id is required"))
		}
		if err := st.SetAPITokenEnabled(ctx, *id, sub == "enable"); err != nil {
			exitErr(sub+" token", err)
		}
		fmt.Printf("OK: token %s %sd\n", *id, sub)
	case "delete":
		if *id == "" {
			exitErr("token id", fmt.Errorf("-id is required"))
		}
		if err := st.DeleteAPIToken(ctx, *id); err != nil {
			exitErr("delete token", err)
		}
		fmt.Printf("OK: token %s deleted\n", *id)
	default:
		apiTokenUsage()
		os.Exit(2)
	}
}

func apiTokenUsage() {
	_, _ = os.Stderr.WriteString(`Usage:
  rclone-syncd apitoken create  [-data DIR] -name NAME [-expires-days N]
  rclone-syncd apitoken list    [-data DIR]
  rclone-syncd apitoken enable  [-data DIR] -id ID
  rclone-syncd apitoken disable [-data DIR] -id ID
  rclone-syncd apitoken delete  [-data DIR] -id ID
`)
}

func openStoreForCLI(dataDir string) *store.Store {
	st, err := store.Open(filepath.Join(dataDir, "115togd.db"))
	if err != nil {
		exitErr("open db", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		exitErr("migrate", err)
	}
	return st
}

func exitErr(what string, err error) {
	_, _ = os.Stderr.WriteString(what + ": " + err.Error() + "\n")
	os.Exit(1)
}

func formatUnix(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}
