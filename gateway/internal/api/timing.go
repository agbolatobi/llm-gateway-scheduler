package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"regexp"
	"time"
)

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type traceKey struct{}

type requestTrace struct {
	selectionSequence                                       uint64
	id                                                      string
	start, dispatch, firstText, complete                    time.Time
	outcome                                                 string
	user, model, runtime, class, usageSource, capacityState string
	reserved, charged                                       int64
	usage                                                   *Usage
	queueWait                                               time.Duration
	schedulerPolicy, dispatchReason                         string
}

type TimingRecord struct {
	Event             string   `json:"event"`
	SelectionSequence uint64   `json:"selection_sequence"`
	QueueWaitMS       float64  `json:"queue_wait_ms"`
	SchedulerPolicy   string   `json:"scheduler_policy,omitempty"`
	DispatchReason    string   `json:"dispatch_reason,omitempty"`
	RequestID         string   `json:"request_id"`
	Outcome           string   `json:"outcome"`
	DispatchMS        *float64 `json:"dispatch_ms"`
	FirstTextMS       *float64 `json:"first_text_ms"`
	CompletionMS      *float64 `json:"completion_ms"`
	EndedMS           float64  `json:"ended_ms"`
	UserID            string   `json:"user_id,omitempty"`
	Model             string   `json:"model,omitempty"`
	RuntimeID         string   `json:"runtime_id,omitempty"`
	RequestClass      string   `json:"request_class,omitempty"`
	ReservedTokens    int64    `json:"reserved_tokens"`
	ChargedTokens     int64    `json:"charged_tokens"`
	UsageSource       string   `json:"usage_source"`
	Usage             *Usage   `json:"usage"`
	CapacityState     string   `json:"capacity_state"`
}

func newRequestTrace(incoming string) (*requestTrace, error) {
	if !safeRequestID.MatchString(incoming) {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		incoming = hex.EncodeToString(random[:])
	}
	return &requestTrace{id: incoming, start: time.Now(), outcome: "rejected", usageSource: "not_reserved", capacityState: "not_acquired"}, nil
}
func requestTraceFromContext(ctx context.Context) *requestTrace {
	trace, _ := ctx.Value(traceKey{}).(*requestTrace)
	return trace
}
func (trace *requestTrace) markFirstText() {
	if trace != nil && trace.firstText.IsZero() {
		trace.firstText = time.Now()
	}
}
func (trace *requestTrace) buildTimingRecord() TimingRecord {
	// Sub uses Go's monotonic clock component; no wall-clock subtraction.
	elapsed := func(at time.Time) *float64 {
		if at.IsZero() {
			return nil
		}
		milliseconds := float64(at.Sub(trace.start)) / float64(time.Millisecond)
		return &milliseconds
	}
	return TimingRecord{
		SelectionSequence: trace.selectionSequence,
		QueueWaitMS:       float64(trace.queueWait) / float64(time.Millisecond),
		SchedulerPolicy:   trace.schedulerPolicy,
		DispatchReason:    trace.dispatchReason,
		Event:             "gateway_request",
		RequestID:         trace.id,
		Outcome:           trace.outcome,
		UserID:            trace.user,
		Model:             trace.model,
		RuntimeID:         trace.runtime,
		RequestClass:      trace.class,
		ReservedTokens:    trace.reserved,
		ChargedTokens:     trace.charged,
		UsageSource:       trace.usageSource,
		Usage:             trace.usage,
		CapacityState:     trace.capacityState,
		DispatchMS:        elapsed(trace.dispatch),
		FirstTextMS:       elapsed(trace.firstText),
		CompletionMS:      elapsed(trace.complete),
		EndedMS:           float64(time.Since(trace.start)) / float64(time.Millisecond),
	}
}
func logTiming(record TimingRecord) {
	raw, _ := json.Marshal(record)
	log.Print(string(raw))
}
