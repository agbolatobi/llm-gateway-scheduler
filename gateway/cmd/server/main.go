package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-gateway-scheduler/gateway/internal/api"
	"llm-gateway-scheduler/gateway/internal/auth"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keys" {
		if err := runKeys(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			log.Fatal(err)
		}
		return
	}
	defaultDir, err := auth.DefaultDir()
	if err != nil {
		log.Fatal(err)
	}
	address := flag.String("listen", "127.0.0.1:8000", "Listen address (local HTTP; keep loopback-only)")
	configDir := flag.String("config-dir", defaultDir, "Directory containing api-keys.json")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected positional arguments")
	}
	keyStore, err := auth.Open(*configDir)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := keyStore.List(); err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              *address,
		Handler:           api.NewHandler(keyStore),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errorsCh := make(chan error, 1)
	go func() {
		log.Printf("Gateway listening on %s (API keys required; inference is a placeholder)", *address)
		errorsCh <- server.ListenAndServe()
	}()
	select {
	case err := <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			log.Printf("Gateway shutdown: %v", err)
		}
	}
}
