package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testManager(t *testing.T, c Config) (*Manager, *time.Time) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m, err := New(c, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return m, &now
}

func TestScalingAndAliases(t *testing.T) {
	c := DefaultConfig()
	c.Models[0].Aliases = []string{"alias-a", "alias-b"}
	for n, want := range []Limits{{30, 100000}, {60, 200000}, {60, 200000}} {
		if n > 0 {
			c.Models = append(c.Models, Model{ID: string(rune('a' + n)), Limits: Limits{30, 100000}})
		}
		got, err := c.EffectiveGlobal()
		if err != nil || got != want {
			t.Fatalf("%d models: %v %v", n+1, got, err)
		}
	}
	if c.User != (Limits{20, 40000}) {
		t.Fatal("user allowances not doubled")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"zero user":       func(c *Config) { c.User.Requests = 0 },
		"negative":        func(c *Config) { c.GlobalCeiling.Tokens = -1 },
		"overflow":        func(c *Config) { c.Models[0].Limits.Tokens = 1 << 62 },
		"duration":        func(c *Config) { c.RequestWindowSeconds = 1 << 62 },
		"no models":       func(c *Config) { c.Models = nil },
		"duplicate ID":    func(c *Config) { c.Models = append(c.Models, c.Models[0]) },
		"alias collision": func(c *Config) { c.Models[0].Aliases = []string{"local-llm"} },
		"empty alias":     func(c *Config) { c.Models[0].Aliases = []string{""} },
		"whitespace":      func(c *Config) { c.Models[0].ID = " m " },
	} {
		t.Run(name, func(t *testing.T) {
			c := DefaultConfig()
			change(&c)
			if _, err := c.EffectiveGlobal(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	file := filepath.Join(t.TempDir(), "quotas.json")
	valid, _ := json.Marshal(DefaultConfig())
	for _, body := range []string{`{}`, `null`, string(valid) + ` {}`, `{"unknown":1}`, `{`} {
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(file); err == nil {
			t.Fatal("accepted invalid file", body)
		}
	}
	if err := os.WriteFile(file, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err != nil {
		t.Fatal(err)
	}
}

func TestRequestLimitsAndWindows(t *testing.T) {
	m, now := testManager(t, DefaultConfig())
	for i := 0; i < 20; i++ {
		if !m.CheckAndCount("alice").Allowed {
			t.Fatal(i)
		}
	}
	if d := m.CheckAndCount("alice"); d.Code != "user_request_limit" || d.RetryAfter != time.Minute {
		t.Fatal(d)
	}
	for i := 0; i < 10; i++ {
		if !m.CheckAndCount("bob").Allowed {
			t.Fatal(i)
		}
	}
	if d := m.CheckAndCount("bob"); d.Code != "global_request_limit" {
		t.Fatal(d)
	}
	if m.requests["bob"] != 10 || m.globalRequests != 30 {
		t.Fatal("partial charge")
	}
	*now = now.Add(time.Minute - time.Nanosecond)
	if d := m.CheckAndCount("bob"); d.Allowed || d.RetryAfter != time.Nanosecond {
		t.Fatal(d)
	}
	*now = now.Add(time.Nanosecond)
	if !m.CheckAndCount("alice").Allowed || m.globalRequests != 1 {
		t.Fatal("window did not reset")
	}
	*now = now.Add(5 * time.Minute)
	if !m.CheckAndCount("alice").Allowed || m.globalRequests != 1 {
		t.Fatal("idle windows did not reset")
	}
	if m.CheckAndCount("").Allowed {
		t.Fatal("empty identity accepted")
	}
}

func TestConcurrentGlobalAndUserLimits(t *testing.T) {
	for _, sameUser := range []bool{true, false} {
		m, _ := testManager(t, DefaultConfig())
		var count atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				user := "alice"
				if !sameUser {
					user = string(rune(1000 + i))
				}
				if m.CheckAndCount(user).Allowed {
					count.Add(1)
				}
			}(i)
		}
		wg.Wait()
		want := int64(30)
		if sameUser {
			want = 20
		}
		if count.Load() != want {
			t.Fatalf("admitted %d want %d", count.Load(), want)
		}
	}
}

func TestReloadPreservesUsage(t *testing.T) {
	c := DefaultConfig()
	m, _ := testManager(t, c)
	for i := 0; i < 20; i++ {
		m.CheckAndCount("alice")
	}
	r, d := m.ReserveTokens("alice", 40000)
	if !d.Allowed {
		t.Fatal(d)
	}
	c.Models = append(c.Models, Model{ID: "second", Limits: Limits{30, 100000}})
	if err := m.Reconfigure(c); err != nil {
		t.Fatal(err)
	}
	if m.globalRequests != 20 || m.globalLimit.Requests != 60 || m.CheckAndCount("alice").Allowed {
		t.Fatal("reload reset usage")
	}
	if available(m.budgets["alice"], m.userLimit.Tokens) != 0 {
		t.Fatal("reload reset holds")
	}
	c.GlobalCeiling.Requests = 10
	if err := m.Reconfigure(c); err != nil {
		t.Fatal(err)
	}
	if d := m.CheckAndCount("bob"); d.Code != "global_request_limit" {
		t.Fatal(d)
	}
	c.RequestWindowSeconds = 5
	if err := m.Reconfigure(c); err == nil || m.globalLimit.Requests != 10 {
		t.Fatal("window reload mutated state")
	}
	if err := m.SettleTokens(r, 10); err != nil {
		t.Fatal(err)
	}
}

func TestTokenBudgetLifecycle(t *testing.T) {
	m, now := testManager(t, DefaultConfig())
	r, d := m.ReserveTokens("alice", 40000)
	if !d.Allowed {
		t.Fatal(d)
	}
	if d = m.CheckAndCount("alice"); d.Code != "user_token_limit" || m.globalRequests != 0 {
		t.Fatal(d)
	}
	if err := m.SettleTokens(r, 10000); err != nil {
		t.Fatal(err)
	}
	if err := m.SettleTokens(r, 0); err == nil {
		t.Fatal("double settlement accepted")
	}
	if available(m.budgets["alice"], 40000) != 30000 || !m.CheckAndCount("alice").Allowed {
		t.Fatal("unused hold not released")
	}
	old, d := m.ReserveTokens("alice", 30000)
	if !d.Allowed {
		t.Fatal(d)
	}
	*now = now.Add(time.Hour)
	current, d := m.ReserveTokens("alice", 40000)
	if !d.Allowed {
		t.Fatal(d)
	}
	if err := m.SettleTokens(old, 20000); err != nil {
		t.Fatal(err)
	}
	if available(m.budgets["alice"], 40000) != 0 {
		t.Fatal("old settlement changed new window")
	}
	if err := m.SettleTokens(current, 0); err != nil {
		t.Fatal(err)
	}
	if !m.CheckAndCount("alice").Allowed {
		t.Fatal("release failed")
	}
}

func TestGlobalTokenExhaustionAndAtomicReserve(t *testing.T) {
	m, _ := testManager(t, DefaultConfig())
	for _, user := range []string{"a", "b"} {
		r, d := m.ReserveTokens(user, 40000)
		if !d.Allowed {
			t.Fatal(d)
		}
		if err := m.SettleTokens(r, 40000); err != nil {
			t.Fatal(err)
		}
	}
	if _, d := m.ReserveTokens("c", 30000); d.Code != "global_token_limit" {
		t.Fatal(d)
	}
	if m.budgets["c"] != nil || m.globalBudget.reserved != 0 {
		t.Fatal("partial reservation")
	}
	_, d := m.ReserveTokens("c", 20000)
	if !d.Allowed {
		t.Fatal(d)
	}
	if d = m.CheckAndCount("d"); d.Code != "global_token_limit" || m.globalRequests != 0 {
		t.Fatal(d)
	}
}

func TestConcurrentReservations(t *testing.T) {
	m, _ := testManager(t, DefaultConfig())
	var count atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, d := m.ReserveTokens(string(rune(1000+i)), 1000)
			if d.Allowed {
				count.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if count.Load() != 100 || m.globalBudget.reserved != 100000 {
		t.Fatal("oversubscribed tokens")
	}
}
