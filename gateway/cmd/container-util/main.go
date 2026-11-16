// container-util provides probes and private storage initialization without a shell.
package main

import (
	"fmt"
	"llm-gateway-scheduler/gateway/internal/auth"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		fail("usage: container-util <init-keys|probe> <path|url>")
	}
	switch os.Args[1] {
	case "init-keys":
		if _, err := auth.Open(os.Args[2]); err != nil {
			fail(err.Error())
		}
	case "probe":
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get(os.Args[2])
		if err != nil {
			fail(err.Error())
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			fail("probe returned non-200 status")
		}
	default:
		fail("unknown container utility command")
	}
}
func fail(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
