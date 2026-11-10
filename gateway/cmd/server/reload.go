package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"llm-gateway-scheduler/gateway/internal/auth"
)

const reloadSocketName = "reload.sock"

type reloadResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Control access is OS-user-only, not granted to ordinary inference API keys.
func checkControlDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("gateway control directory unavailable; start the updated gateway with the same --config-dir: %w", err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("gateway control directory must be owned by this user and private (chmod 700)")
	}
	return nil
}

func checkControlSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe gateway control socket")
	}
	return nil
}

func reloadHandler(reload func() error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/reload" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := reload(); err != nil {
			log.Printf("Configuration reload rejected; previous configuration retained: %v", err)
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(reloadResponse{Message: "Reload rejected; previous configuration retained: " + err.Error()})
			return
		}
		json.NewEncoder(w).Encode(reloadResponse{OK: true, Message: "Gateway configuration reloaded. Quota usage preserved; model processes unchanged."})
	})
}

func startReloadControl(dir string, reload func() error) (func(), error) {
	if err := checkControlDir(dir); err != nil {
		return nil, err
	}
	// Hold a separate lifetime lock so another gateway cannot steal the socket.
	fd, err := syscall.Open(filepath.Join(dir, "reload.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "reload.lock")
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		lock.Close()
		return nil, fmt.Errorf("unsafe gateway control lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another gateway uses this --config-dir; use a separate directory for each gateway")
	}
	ready := false
	defer func() {
		if !ready {
			lock.Close()
		}
	}()
	path := filepath.Join(dir, reloadSocketName)
	if err := checkControlSocket(path); err == nil {
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("gateway control socket is already active")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot verify stale control socket: %w", dialErr)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	server := &http.Server{Handler: reloadHandler(reload), ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 2 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Local reload control stopped: %v", err)
		}
	}()
	ready = true
	return func() {
		server.Close()
		listener.Close()
		lock.Close()
	}, nil
}

func runReload(args []string, output io.Writer) error {
	defaultDir, err := auth.DefaultDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("gateway reload", flag.ContinueOnError)
	flags.SetOutput(output)
	dir := flags.String("config-dir", defaultDir, "Same private directory used by the running gateway")
	timeout := flags.Duration("timeout", 10*time.Second, "Maximum wait for confirmation (e.g. 10s)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 || *timeout > time.Minute {
		return fmt.Errorf("reload accepts no positional arguments; --timeout must be greater than zero and at most 1m")
	}
	if err := checkControlDir(*dir); err != nil {
		return err
	}
	path := filepath.Join(*dir, reloadSocketName)
	if err := checkControlSocket(path); err != nil {
		return fmt.Errorf("gateway reload unavailable; start/restart the updated gateway with the same --config-dir: %w", err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: *timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Post("http://gateway-control/reload", "application/json", nil)
	if err != nil {
		return fmt.Errorf("could not confirm reload; check the gateway is running with the same --config-dir and inspect its log (a timeout does not prove rejection): %w", err)
	}
	defer response.Body.Close()
	var result reloadResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&result); err != nil {
		return fmt.Errorf("invalid gateway reload response; outcome unconfirmed: %w", err)
	}
	if response.StatusCode != http.StatusOK || !result.OK {
		return fmt.Errorf("%s", result.Message)
	}
	_, err = fmt.Fprintln(output, result.Message)
	return err
}
