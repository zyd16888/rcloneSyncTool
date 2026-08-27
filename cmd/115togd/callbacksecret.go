package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"115togd/internal/store"
)

// runCallbackSecret manages the shared HMAC secret used to sign transfer
// callbacks. The same value must be configured on the receiving side; it is
// never transmitted, so it is minted and shown on a terminal rather than
// rendered into a browser page.
func runCallbackSecret(args []string) {
	if len(args) == 0 {
		callbackSecretUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("callbacksecret "+sub, flag.ExitOnError)
	dataDir := fs.String("data", "./data", "Data directory")
	fromStdin := fs.Bool("stdin", false, "Read the secret from stdin (set)")
	_ = fs.Parse(args[1:])

	st := openStoreForCLI(*dataDir)
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "rotate":
		secret, err := store.NewCallbackSecret()
		if err != nil {
			exitErr("generate secret", err)
		}
		if err := st.SetCallbackSecret(ctx, secret); err != nil {
			exitErr("save secret", err)
		}
		fmt.Printf("OK: callback secret rotated\n  secret: %s\n", secret)
		fmt.Println("\n请把该值填入接收端的回调签名密钥，它不会再次显示。")
	case "set":
		var secret string
		if *fromStdin {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				exitErr("read stdin", err)
			}
			secret = strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))
		} else {
			rest := fs.Args()
			if len(rest) != 1 {
				callbackSecretUsage()
				os.Exit(2)
			}
			secret = strings.TrimSpace(rest[0])
		}
		if len(secret) < 16 {
			exitErr("callback secret", fmt.Errorf("secret must be at least 16 characters"))
		}
		if err := st.SetCallbackSecret(ctx, secret); err != nil {
			exitErr("save secret", err)
		}
		fmt.Println("OK: callback secret updated")
	case "clear":
		if err := st.SetCallbackSecret(ctx, ""); err != nil {
			exitErr("clear secret", err)
		}
		fmt.Println("OK: callback secret cleared; callbacks will not be sent")
	case "status":
		secret, err := st.CallbackSecret(ctx)
		if err != nil {
			exitErr("read secret", err)
		}
		if secret == "" {
			fmt.Println("callback secret: not configured (callbacks disabled)")
			return
		}
		fmt.Printf("callback secret: configured (%d characters)\n", len(secret))
	default:
		callbackSecretUsage()
		os.Exit(2)
	}
}

func callbackSecretUsage() {
	_, _ = os.Stderr.WriteString(`Usage:
  rclone-syncd callbacksecret rotate [-data DIR]
  rclone-syncd callbacksecret set    [-data DIR] [-stdin] <secret>
  rclone-syncd callbacksecret clear  [-data DIR]
  rclone-syncd callbacksecret status [-data DIR]
`)
}
