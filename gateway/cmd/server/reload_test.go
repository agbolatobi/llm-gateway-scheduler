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

func controlDir(testContext *testing.T) string {
	testContext.Helper()
	// Unix socket paths have a short OS length limit.
	dir, err := os.MkdirTemp("", "gw-reload-")
	if err != nil {
		testContext.Fatal(err)
	}
	testContext.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestReloadCommand(testContext *testing.T) {
	dir := controlDir(testContext)
	path := filepath.Join(dir, "config.json")
	config := registry.Default()
	write := func() {
		raw, _ := json.Marshal(config)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			testContext.Fatal(err)
		}
	}
	write()
	handler, _, reload, err := configureHandler(configVerifier{}, path, "", "")
	if err != nil {
		testContext.Fatal(err)
	}
	closeControl, err := startReloadControl(dir, reload)
	if err != nil {
		testContext.Fatal(err)
	}
	defer closeControl()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		httpRequest := httptest.NewRequest(method, path, strings.NewReader(body))
		httpRequest.Header.Set("Authorization", "Bearer test")
		httpRequest.Header.Set("Content-Type", "application/json")
		responseRecorder := httptest.NewRecorder()
		handler.ServeHTTP(responseRecorder, httpRequest)
		return responseRecorder
	}
	body := `{"model":"unknown","messages":[{"role":"user","content":"hello"}]}`
	if responseRecorder := request("POST", "/v1/chat/completions", body); responseRecorder.Code != 404 {
		testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
	}
	second := config.Models[0]
	second.ID, second.Aliases = "second", nil
	second.Backend.RuntimeID, second.Backend.BaseURL = "second", "http://127.0.0.1:8082"
	config.Models = append(config.Models, second)
	config.Quotas.User.Requests = 1
	write()
	var output bytes.Buffer
	if err := runReload([]string{"--config-dir", dir}, &output); err != nil {
		testContext.Fatal(err)
	}
	if !strings.Contains(output.String(), "reloaded") {
		testContext.Fatal(output.String())
	}
	if responseRecorder := request("GET", "/v1/models", ""); responseRecorder.Code != 200 || !strings.Contains(responseRecorder.Body.String(), "second") {
		testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
	}
	if responseRecorder := request("POST", "/v1/chat/completions", body); responseRecorder.Code != 429 {
		testContext.Fatal("reload reset quota usage", responseRecorder.Code)
	}
	// The administrative action is not exposed on the inference HTTP handler.
	if responseRecorder := request("POST", "/reload", ""); responseRecorder.Code != 404 {
		testContext.Fatal(responseRecorder.Code)
	}
	os.WriteFile(path, []byte("{"), 0600)
	output.Reset()
	if err := runReload([]string{"--config-dir", dir}, &output); err == nil || !strings.Contains(err.Error(), "previous configuration retained") {
		testContext.Fatal(err)
	}
	if output.Len() != 0 {
		testContext.Fatal("rejected reload printed success")
	}
	if responseRecorder := request("GET", "/v1/models", ""); !strings.Contains(responseRecorder.Body.String(), "second") {
		testContext.Fatal("invalid reload replaced registry")
	}
	config.Gateway.Listen = "127.0.0.1:8001"
	write()
	if err := runReload([]string{"--config-dir", dir}, &output); err == nil || !strings.Contains(err.Error(), "require restart") {
		testContext.Fatal(err)
	}
}

func TestControlOwnershipAndCleanup(testContext *testing.T) {
	dir := controlDir(testContext)
	closeControl, err := startReloadControl(dir, func() error { return nil })
	if err != nil {
		testContext.Fatal(err)
	}
	testContext.Cleanup(closeControl)
	if closeOther, err := startReloadControl(dir, func() error { return nil }); err == nil {
		closeOther()
		testContext.Fatal("second server stole control socket")
	}
	if err := runReload([]string{"--config-dir", dir}, &bytes.Buffer{}); err != nil {
		testContext.Fatal(err)
	}
	closeControl()
	if _, err := os.Lstat(filepath.Join(dir, reloadSocketName)); !os.IsNotExist(err) {
		testContext.Fatal("socket not cleaned", err)
	}
	if err := runReload([]string{"--config-dir", dir}, &bytes.Buffer{}); err == nil {
		testContext.Fatal("stopped gateway reported success")
	}
	// Simulate a socket left after a process crash; the new owner can recover it.
	socket, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, reloadSocketName), Net: "unix"})
	if err != nil {
		testContext.Fatal(err)
	}
	socket.SetUnlinkOnClose(false)
	os.Chmod(filepath.Join(dir, reloadSocketName), 0600)
	socket.Close()
	closeNext, err := startReloadControl(dir, func() error { return nil })
	if err != nil {
		testContext.Fatal(err)
	}
	closeNext()
}

func TestUnsafeControlPaths(testContext *testing.T) {
	for _, kind := range []string{"public directory", "regular socket path", "symlink socket", "symlink lock"} {
		testContext.Run(kind, func(testContext *testing.T) {
			dir := controlDir(testContext)
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
				testContext.Fatal("unsafe path accepted")
			}
			if raw, _ := os.ReadFile(target); string(raw) != "keep" {
				testContext.Fatal("existing file modified")
			}
		})
	}
}

func TestReloadFailureModes(testContext *testing.T) {
	dir := controlDir(testContext)
	_, _, reload, err := configureHandler(configVerifier{}, "", "", "")
	if err != nil {
		testContext.Fatal(err)
	}
	closeControl, err := startReloadControl(dir, reload)
	if err != nil {
		testContext.Fatal(err)
	}
	defer closeControl()
	if err := runReload([]string{"--config-dir", dir}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no --config") {
		testContext.Fatal(err)
	}
	for _, args := range [][]string{{"unexpected"}, {"--timeout", "0s"}, {"--timeout", "2m"}} {
		if err := runReload(args, &bytes.Buffer{}); err == nil {
			testContext.Fatal("bad arguments accepted")
		}
	}
	if err := runReload([]string{"--help"}, &bytes.Buffer{}); err != nil {
		testContext.Fatal(err)
	}
	called := false
	responseRecorder := httptest.NewRecorder()
	reloadHandler(func() error { called = true; return nil }).ServeHTTP(responseRecorder, httptest.NewRequest(http.MethodGet, "/reload", nil))
	if responseRecorder.Code != 405 || called {
		testContext.Fatal("GET triggered reload")
	}
}

func TestReloadTimeout(testContext *testing.T) {
	dir := controlDir(testContext)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	closeControl, err := startReloadControl(dir, func() error {
		close(started)
		<-release
		close(finished)
		return nil
	})
	if err != nil {
		testContext.Fatal(err)
	}
	defer closeControl()
	result := make(chan error, 1)
	go func() { result <- runReload([]string{"--config-dir", dir, "--timeout", "100ms"}, &bytes.Buffer{}) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		testContext.Fatal("reload did not start")
	}
	err = <-result
	close(release)
	<-finished
	if err == nil || !strings.Contains(err.Error(), "could not confirm") {
		testContext.Fatal(err)
	}
}

func TestGatewayCommandProcess(testContext *testing.T) {
	if os.Getenv("GATEWAY_RELOAD_TEST_HELPER") == "1" {
		for index, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"gateway"}, os.Args[index+1:]...)
				break
			}
		}
		flag.CommandLine = flag.NewFlagSet("gateway", flag.ExitOnError)
		main()
		os.Exit(0)
	}
	dir := controlDir(testContext)
	path := filepath.Join(dir, "gateway.json")
	config := registry.Default()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		testContext.Fatal(err)
	}
	config.Gateway.Listen = probe.Addr().String()
	probe.Close()
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		testContext.Fatal(err)
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
		testContext.Fatal(err)
	}
	testContext.Cleanup(func() {
		server.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- server.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				testContext.Errorf("temporary gateway exited: %v\n%s", err, logs.String())
			}
		case <-time.After(5 * time.Second):
			server.Process.Kill()
			<-done
			testContext.Error("temporary gateway did not shut down")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		output, err := command("reload", "--config-dir", dir).CombinedOutput()
		if err == nil {
			if !strings.Contains(string(output), "configuration reloaded") {
				testContext.Fatal(string(output))
			}
			break
		}
		if time.Now().After(deadline) {
			testContext.Fatalf("command failed: %v %s", err, output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		testContext.Fatal(err)
	}
	output, err := command("reload", "--config-dir", dir).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "previous configuration retained") {
		testContext.Fatalf("rejection exit status/output: %v %s", err, output)
	}
}
