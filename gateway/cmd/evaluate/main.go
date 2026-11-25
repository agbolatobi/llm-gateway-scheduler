// evaluate is a portable worker. The Python command builds and transports it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"llm-gateway-scheduler/gateway/internal/evaluation"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 8<<20))
	decoder.DisallowUnknownFields()
	var plan evaluation.Plan
	if err := decoder.Decode(&plan); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		fmt.Fprintln(os.Stderr, "expected one plan")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	recorder := evaluation.NewRecorder(os.Stdout)
	if err := evaluation.Run(ctx, plan, recorder); err != nil {
		recorder.Emit("run_error", map[string]any{"error": err.Error()})
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
