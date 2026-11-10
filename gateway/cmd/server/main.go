package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"llm-gateway-scheduler/gateway/internal/api"
	"llm-gateway-scheduler/gateway/internal/auth"
	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/registry"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "reload" {
		if err := runReload(os.Args[2:], os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
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
	address := flag.String("listen", "", "Override configured listen address (loopback only)")
	configPath := flag.String("config", "", "Combined gateway/model/quota JSON; defaults to built-in local-llm registration")
	configDir := flag.String("config-dir", defaultDir, "Directory containing api-keys.json")
	quotaPath := flag.String("quota-config", "", "Quota JSON file; defaults to built-in local-llm settings. SIGHUP reloads this file.")
	proxyOptions := api.DefaultProxyOptions()
	requestTimeout := flag.Duration("request-timeout", proxyOptions.RequestTimeout, "Overall chat request limit, including validation")
	idleTimeout := flag.Duration("backend-idle-timeout", proxyOptions.IdleTimeout, "Maximum wait for backend body data")
	writeTimeout := flag.Duration("client-write-timeout", proxyOptions.WriteTimeout, "Maximum wait for each client write")
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
	proxyOptions = api.ProxyOptions{RequestTimeout: *requestTimeout, IdleTimeout: *idleTimeout, WriteTimeout: *writeTimeout}
	if err := proxyOptions.Validate(); err != nil {
		log.Fatal(err)
	}
	handler, listen, reloadConfig, err := configureHandler(keyStore, *configPath, *quotaPath, *address, proxyOptions)
	if err != nil {
		log.Fatal(err)
	}
	if closer, ok := handler.(interface{ Close() }); ok {
		defer closer.Close()
	}
	// Serialize command and SIGHUP reloads, including reading the config file.
	var reloadMu sync.Mutex
	loadConfig := reloadConfig
	reloadConfig = func() error {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		return loadConfig()
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	closeControl, err := startReloadControl(keyStore.Dir, reloadConfig)
	if err != nil {
		listener.Close()
		log.Fatal(err)
	}
	defer closeControl()
	server := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Chat writes use per-write deadlines so a healthy stream can exceed 15s.
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)
	errorsCh := make(chan error, 1)
	go func() {
		log.Printf("Gateway listening on %s (API keys required)", listen)
		errorsCh <- server.Serve(listener)
	}()
	for {
		select {
		case <-reload:
			if err := reloadConfig(); err != nil {
				log.Printf("Configuration reload rejected; previous configuration retained: %v", err)
			}
		case err := <-errorsCh:
			if !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
			return
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
				log.Printf("Gateway shutdown: %v", err)
			}
			return
		}
	}
}

func configureHandler(verifier api.Authenticator, configPath, quotaPath, address string, options ...api.ProxyOptions) (http.Handler, string, func() error, error) {
	if configPath != "" && quotaPath != "" {
		return nil, "", nil, fmt.Errorf("use --config or legacy --quota-config, not both")
	}
	// Retain the old quota-only flag for existing benchmark/dev commands. It does
	// not pretend those metadata-only entries are verified model registrations.
	if quotaPath != "" {
		path, err := filepath.Abs(quotaPath)
		if err != nil {
			return nil, "", nil, err
		}
		c, err := quota.Load(path)
		if err != nil {
			return nil, "", nil, err
		}
		q, err := quota.New(c, nil)
		if err != nil {
			return nil, "", nil, err
		}
		if address == "" {
			address = "127.0.0.1:8000"
		}
		log.Print("Legacy --quota-config mode has no model registry or model-budget validation; use --config for GATE-04")
		logQuotaLimits(c)
		return api.NewHandler(verifier, q), address, func() error {
			updated, err := quota.Load(path)
			if err == nil {
				err = q.Reconfigure(updated)
			}
			if err == nil {
				logQuotaLimits(updated)
			}
			return err
		}, nil
	}
	path := ""
	if configPath != "" {
		var err error
		path, err = filepath.Abs(configPath)
		if err != nil {
			return nil, "", nil, err
		}
	}
	load := func() (registry.Config, error) {
		c := registry.Default()
		var err error
		if path != "" {
			c, err = registry.Load(path)
			if err != nil {
				return c, err
			}
		}
		if address != "" {
			c.Gateway.Listen = address
		}
		return c, c.Validate()
	}
	c, err := load()
	if err != nil {
		return nil, "", nil, err
	}
	proxyOptions := api.DefaultProxyOptions()
	if len(options) > 0 {
		proxyOptions = options[0]
	}
	h, err := api.NewRegisteredHandlerWithOptions(verifier, c, nil, proxyOptions)
	if err != nil {
		return nil, "", nil, err
	}
	logQuotaLimits(c.QuotaConfig())
	return h, c.Gateway.Listen, func() error {
		if path == "" {
			return fmt.Errorf("no --config file supplied; built-in registrations remain active")
		}
		updated, err := load()
		if err == nil {
			err = h.Reload(updated)
		}
		if err == nil {
			logQuotaLimits(updated.QuotaConfig())
		}
		return err
	}, nil
}

func logQuotaLimits(c quota.Config) {
	g, _ := c.EffectiveGlobal() // Only called after validation.
	log.Printf("Quota limits: user=%d requests/%ds, %d tokens/%ds; global=%d requests/%ds, %d tokens/%ds (single process)",
		c.User.Requests, c.RequestWindowSeconds, c.User.Tokens, c.TokenWindowSeconds,
		g.Requests, c.RequestWindowSeconds, g.Tokens, c.TokenWindowSeconds)
}
