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
	id                                                      string
	start, dispatch, firstText, complete                    time.Time
	outcome                                                 string
	user, model, runtime, class, usageSource, capacityState string
	reserved, charged                                       int64
	usage                                                   *Usage
}

type TimingRecord struct {
	Event          string   `json:"event"`
	RequestID      string   `json:"request_id"`
	Outcome        string   `json:"outcome"`
	DispatchMS     *float64 `json:"dispatch_ms"`
	FirstTextMS    *float64 `json:"first_text_ms"`
	CompletionMS   *float64 `json:"completion_ms"`
	EndedMS        float64  `json:"ended_ms"`
	UserID         string   `json:"user_id,omitempty"`
	Model          string   `json:"model,omitempty"`
	RuntimeID      string   `json:"runtime_id,omitempty"`
	RequestClass   string   `json:"request_class,omitempty"`
	ReservedTokens int64    `json:"reserved_tokens"`
	ChargedTokens  int64    `json:"charged_tokens"`
	UsageSource    string   `json:"usage_source"`
	Usage          *Usage   `json:"usage"`
	CapacityState  string   `json:"capacity_state"`
}

func newTrace(incoming string) (*requestTrace, error) {
	if !safeRequestID.MatchString(incoming) {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		incoming = hex.EncodeToString(random[:])
	}
	return &requestTrace{id: incoming, start: time.Now(), outcome: "rejected", usageSource: "not_reserved", capacityState: "not_acquired"}, nil
}
func traceFrom(ctx context.Context) *requestTrace {
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	return t
}
func (t *requestTrace) text() {
	if t != nil && t.firstText.IsZero() {
		t.firstText = time.Now()
	}
}
func (t *requestTrace) record() TimingRecord {
	// Sub uses Go's monotonic clock component; no wall-clock subtraction.
	elapsed := func(at time.Time) *float64 {
		if at.IsZero() {
			return nil
		}
		v := float64(at.Sub(t.start)) / float64(time.Millisecond)
		return &v
	}
	return TimingRecord{Event: "gateway_request", RequestID: t.id, Outcome: t.outcome, UserID: t.user, Model: t.model, RuntimeID: t.runtime, RequestClass: t.class, ReservedTokens: t.reserved, ChargedTokens: t.charged, UsageSource: t.usageSource, Usage: t.usage, CapacityState: t.capacityState,
		DispatchMS: elapsed(t.dispatch), FirstTextMS: elapsed(t.firstText), CompletionMS: elapsed(t.complete), EndedMS: float64(time.Since(t.start)) / float64(time.Millisecond)}
}
func logTiming(record TimingRecord) { raw, _ := json.Marshal(record); log.Print(string(raw)) }
