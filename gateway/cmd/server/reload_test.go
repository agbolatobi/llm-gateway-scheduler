package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func controlDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths have a short OS length limit.
	dir, err := os.MkdirTemp("", "gw-reload-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestReloadCommand(t *testing.T) {
	dir := controlDir(t)
	path := filepath.Join(dir, "config.json")
	c := registry.Default()
	write := func() {
		raw, _ := json.Marshal(c)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	h, _, reload, err := configureHandler(configVerifier{}, path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	closeControl, err := startReloadControl(dir, reload)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"model":"unknown","messages":[{"role":"user","content":"hello"}]}`
	if w := request("POST", "/v1/chat/completions", body); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	second := c.Models[0]
	second.ID, second.Aliases = "second", nil
	second.Backend.RuntimeID, second.Backend.BaseURL = "second", "http://127.0.0.1:8082"
	c.Models = append(c.Models, second)
	c.Quotas.User.Requests = 1
	write()
	var output bytes.Buffer
	if err := runReload([]string{"--config-dir", dir}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "reloaded") {
		t.Fatal(output.String())
	}
	if w := request("GET", "/v1/models", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "second") {
		t.Fatal(w.Code, w.Body)
	}
	if w := request("POST", "/v1/chat/completions", body); w.Code != 429 {
		t.Fatal("reload reset quota usage", w.Code)
	}
	// The administrative action is not exposed on the inference HTTP handler.
	if w := request("POST", "/reload", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	os.WriteFile(path, []byte("{"), 0600)
	output.Reset()
	if err := runReload([]string{"--config-dir", dir}, &output); err == nil || !strings.Contains(err.Error(), "previous configuration retained") {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("rejected reload printed success")
	}
	if w := request("GET", "/v1/models", ""); !strings.Contains(w.Body.String(), "second") {
		t.Fatal("invalid reload replaced registry")
	}
	c.Gateway.Listen = "127.0.0.1:8001"
	write()
	if err := runReload([]string{"--config-dir", dir}, &output); err == nil || !strings.Contains(err.Error(), "require restart") {
		t.Fatal(err)
	}
}

func TestControlOwnershipAndCleanup(t *testing.T) {
	dir := controlDir(t)
	closeControl, err := startReloadControl(dir, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeControl)
	if closeOther, err := startReloadControl(dir, func() error { return nil }); err == nil {
		closeOther()
		t.Fatal("second server stole control socket")
	}
	if err := runReload([]string{"--config-dir", dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	closeControl()
	if _, err := os.Lstat(filepath.Join(dir, reloadSocketName)); !os.IsNotExist(err) {
		t.Fatal("socket not cleaned", err)
	}
	if err := runReload([]string{"--config-dir", dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("stopped gateway reported success")
	}
	// Simulate a socket left after a process crash; the new owner can recover it.
	socket, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, reloadSocketName), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	socket.SetUnlinkOnClose(false)
	os.Chmod(filepath.Join(dir, reloadSocketName), 0600)
	socket.Close()
	closeNext, err := startReloadControl(dir, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	closeNext()
}

func TestUnsafeControlPaths(t *testing.T) {
	for _, kind := range []string{"public directory", "regular socket path", "symlink socket", "symlink lock"} {
		t.Run(kind, func(t *testing.T) {
			dir := controlDir(t)
			path := filepath.Join(dir, reloadSocketName)
			target := filepath.Join(dir, "keep")
			os.WriteFile(target, []byte("keep"), 0600)
			switch kind {
			case "public directory":
				os.Chmod(dir, 0755)
			case "regular socket path":
				os.WriteFile(path, []byte("keep"), 0600)
			case "symlink socket":
				os.Symlink(target, path)
			case "symlink lock":
				os.Symlink(target, filepath.Join(dir, "reload.lock"))
			}
			if closeControl, err := startReloadControl(dir, func() error { return nil }); err == nil {
				closeControl()
				t.Fatal("unsafe path accepted")
			}
			if raw, _ := os.ReadFile(target); string(raw) != "keep" {
				t.Fatal("existing file modified")
			}
		})
	}
}

func TestReloadFailureModes(t *testing.T) {
	dir := controlDir(t)
	_, _, reload, err := configureHandler(configVerifier{}, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	closeControl, err := startReloadControl(dir, reload)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	if err := runReload([]string{"--config-dir", dir}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no --config") {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"unexpected"}, {"--timeout", "0s"}, {"--timeout", "2m"}} {
		if err := runReload(args, &bytes.Buffer{}); err == nil {
			t.Fatal("bad arguments accepted")
		}
	}
	if err := runReload([]string{"--help"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	called := false
	w := httptest.NewRecorder()
	reloadHandler(func() error { called = true; return nil }).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/reload", nil))
	if w.Code != 405 || called {
		t.Fatal("GET triggered reload")
	}
}

func TestReloadTimeout(t *testing.T) {
	dir := controlDir(t)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	closeControl, err := startReloadControl(dir, func() error {
		close(started)
		<-release
		close(finished)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	result := make(chan error, 1)
	go func() { result <- runReload([]string{"--config-dir", dir, "--timeout", "100ms"}, &bytes.Buffer{}) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("reload did not start")
	}
	err = <-result
	close(release)
	<-finished
	if err == nil || !strings.Contains(err.Error(), "could not confirm") {
		t.Fatal(err)
	}
}

func TestGatewayCommandProcess(t *testing.T) {
	if os.Getenv("GATEWAY_RELOAD_TEST_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"gateway"}, os.Args[i+1:]...)
				break
			}
		}
		flag.CommandLine = flag.NewFlagSet("gateway", flag.ExitOnError)
		main()
		os.Exit(0)
	}
	dir := controlDir(t)
	path := filepath.Join(dir, "gateway.json")
	c := registry.Default()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.Gateway.Listen = probe.Addr().String()
	probe.Close()
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestGatewayCommandProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "GATEWAY_RELOAD_TEST_HELPER=1")
		return cmd
	}
	server := command("--config", path, "--config-dir", dir)
	var logs bytes.Buffer
	server.Stdout, server.Stderr = &logs, &logs
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- server.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("temporary gateway exited: %v\n%s", err, logs.String())
			}
		case <-time.After(5 * time.Second):
			server.Process.Kill()
			<-done
			t.Error("temporary gateway did not shut down")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		output, err := command("reload", "--config-dir", dir).CombinedOutput()
		if err == nil {
			if !strings.Contains(string(output), "configuration reloaded") {
				t.Fatal(string(output))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("command failed: %v %s", err, output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := command("reload", "--config-dir", dir).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "previous configuration retained") {
		t.Fatalf("rejection exit status/output: %v %s", err, output)
	}
}
