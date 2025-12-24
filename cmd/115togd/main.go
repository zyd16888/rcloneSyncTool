package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"115togd/internal/daemon"
	"115togd/internal/server"
	"115togd/internal/store"
)

func main() {
	var (
		listenAddr = flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
		dataDir    = flag.String("data", "./data", "Data directory")
	)
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("mkdir data dir: %v", err)
	}

	dbPath := filepath.Join(*dataDir, "115togd.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer st.Close()

	if err := st.Migrate(context.Background()); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	logDir := filepath.Join(*dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		log.Fatalf("mkdir log dir: %v", err)
	}

	setDefaults := store.DefaultSettings{
		RcloneConfigPath: "",
		LogDir:           logDir,
		RcPortStart:      55720,
		RcPortEnd:        55800,
		GlobalMaxJobs:    0,
		Transfers:        4,
		Checkers:         8,
		BufferSize:       "64M",
		DriveChunkSize:   "64M",
		Bwlimit:          "",
		MetricsInterval:  2 * time.Second,
		SchedulerTick:    2 * time.Second,
	}
	if err := st.EnsureDefaultSettings(context.Background(), setDefaults); err != nil {
		log.Fatalf("init settings: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := daemon.RecoverDanglingRuns(ctx, st); err != nil {
		log.Fatalf("recover: %v", err)
	}

	supervisor := daemon.NewSupervisor(st)
	go supervisor.Run(ctx)

	handler := server.New(st, supervisor, logDir)

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on http://%s", srv.Addr)

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	log.Printf("shutting down...")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}
