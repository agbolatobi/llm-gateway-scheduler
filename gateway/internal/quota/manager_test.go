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

func testManager(testContext *testing.T, config Config) (*Manager, *time.Time) {
	testContext.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	quotaManager, err := New(config, func() time.Time { return now })
	if err != nil {
		testContext.Fatal(err)
	}
	return quotaManager, &now
}

func TestScalingAndAliases(testContext *testing.T) {
	config := DefaultConfig()
	config.Models[0].Aliases = []string{"alias-a", "alias-b"}
	for count, want := range []Limits{{30, 100000}, {60, 200000}, {60, 200000}} {
		if count > 0 {
			config.Models = append(config.Models, Model{ID: string(rune('a' + count)), Limits: Limits{30, 100000}})
		}
		got, err := config.EffectiveGlobal()
		if err != nil || got != want {
			testContext.Fatalf("%d models: %v %v", count+1, got, err)
		}
	}
	if config.User != (Limits{20, 40000}) {
		testContext.Fatal("user allowances not doubled")
	}
}

func TestConfigValidation(testContext *testing.T) {
	for name, change := range map[string]func(*Config){
		"zero user":       func(config *Config) { config.User.Requests = 0 },
		"negative":        func(config *Config) { config.GlobalCeiling.Tokens = -1 },
		"overflow":        func(config *Config) { config.Models[0].Limits.Tokens = 1 << 62 },
		"duration":        func(config *Config) { config.RequestWindowSeconds = 1 << 62 },
		"no models":       func(config *Config) { config.Models = nil },
		"duplicate ID":    func(config *Config) { config.Models = append(config.Models, config.Models[0]) },
		"alias collision": func(config *Config) { config.Models[0].Aliases = []string{"local-llm"} },
		"empty alias":     func(config *Config) { config.Models[0].Aliases = []string{""} },
		"whitespace":      func(config *Config) { config.Models[0].ID = " m " },
	} {
		testContext.Run(name, func(testContext *testing.T) {
			config := DefaultConfig()
			change(&config)
			if _, err := config.EffectiveGlobal(); err == nil {
				testContext.Fatal("accepted invalid config")
			}
		})
	}
	file := filepath.Join(testContext.TempDir(), "quotas.json")
	valid, _ := json.Marshal(DefaultConfig())
	for _, body := range []string{`{}`, `null`, string(valid) + ` {}`, `{"unknown":1}`, `{`} {
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			testContext.Fatal(err)
		}
		if _, err := Load(file); err == nil {
			testContext.Fatal("accepted invalid file", body)
		}
	}
	if err := os.WriteFile(file, valid, 0600); err != nil {
		testContext.Fatal(err)
	}
	if _, err := Load(file); err != nil {
		testContext.Fatal(err)
	}
}

func TestRequestLimitsAndWindows(testContext *testing.T) {
	quotaManager, now := testManager(testContext, DefaultConfig())
	for index := 0; index < 20; index++ {
		if !quotaManager.CheckAndCount("alice").Allowed {
			testContext.Fatal(index)
		}
	}
	if quotaDecision := quotaManager.CheckAndCount("alice"); quotaDecision.Code != "user_request_limit" || quotaDecision.RetryAfter != time.Minute {
		testContext.Fatal(quotaDecision)
	}
	for index := 0; index < 10; index++ {
		if !quotaManager.CheckAndCount("bob").Allowed {
			testContext.Fatal(index)
		}
	}
	if quotaDecision := quotaManager.CheckAndCount("bob"); quotaDecision.Code != "global_request_limit" {
		testContext.Fatal(quotaDecision)
	}
	if quotaManager.requests["bob"] != 10 || quotaManager.globalRequests != 30 {
		testContext.Fatal("partial charge")
	}
	*now = now.Add(time.Minute - time.Nanosecond)
	if quotaDecision := quotaManager.CheckAndCount("bob"); quotaDecision.Allowed || quotaDecision.RetryAfter != time.Nanosecond {
		testContext.Fatal(quotaDecision)
	}
	*now = now.Add(time.Nanosecond)
	if !quotaManager.CheckAndCount("alice").Allowed || quotaManager.globalRequests != 1 {
		testContext.Fatal("window did not reset")
	}
	*now = now.Add(5 * time.Minute)
	if !quotaManager.CheckAndCount("alice").Allowed || quotaManager.globalRequests != 1 {
		testContext.Fatal("idle windows did not reset")
	}
	if quotaManager.CheckAndCount("").Allowed {
		testContext.Fatal("empty identity accepted")
	}
}

func TestConcurrentGlobalAndUserLimits(testContext *testing.T) {
	for _, sameUser := range []bool{true, false} {
		quotaManager, _ := testManager(testContext, DefaultConfig())
		var count atomic.Int64
		var wg sync.WaitGroup
		for index := 0; index < 200; index++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				user := "alice"
				if !sameUser {
					user = string(rune(1000 + index))
				}
				if quotaManager.CheckAndCount(user).Allowed {
					count.Add(1)
				}
			}(index)
		}
		wg.Wait()
		want := int64(30)
		if sameUser {
			want = 20
		}
		if count.Load() != want {
			testContext.Fatalf("admitted %d want %d", count.Load(), want)
		}
	}
}

func TestReloadPreservesUsage(testContext *testing.T) {
	config := DefaultConfig()
	quotaManager, _ := testManager(testContext, config)
	for index := 0; index < 20; index++ {
		quotaManager.CheckAndCount("alice")
	}
	reservation, quotaDecision := quotaManager.ReserveTokens("alice", 40000)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	config.Models = append(config.Models, Model{ID: "second", Limits: Limits{30, 100000}})
	if err := quotaManager.Reconfigure(config); err != nil {
		testContext.Fatal(err)
	}
	if quotaManager.globalRequests != 20 || quotaManager.globalLimit.Requests != 60 || quotaManager.CheckAndCount("alice").Allowed {
		testContext.Fatal("reload reset usage")
	}
	if available(quotaManager.budgets["alice"], quotaManager.userLimit.Tokens) != 0 {
		testContext.Fatal("reload reset holds")
	}
	config.GlobalCeiling.Requests = 10
	if err := quotaManager.Reconfigure(config); err != nil {
		testContext.Fatal(err)
	}
	if quotaDecision := quotaManager.CheckAndCount("bob"); quotaDecision.Code != "global_request_limit" {
		testContext.Fatal(quotaDecision)
	}
	config.RequestWindowSeconds = 5
	if err := quotaManager.Reconfigure(config); err == nil || quotaManager.globalLimit.Requests != 10 {
		testContext.Fatal("window reload mutated state")
	}
	if err := quotaManager.SettleTokens(reservation, 10); err != nil {
		testContext.Fatal(err)
	}
}

func TestTokenBudgetLifecycle(testContext *testing.T) {
	quotaManager, now := testManager(testContext, DefaultConfig())
	reservation, quotaDecision := quotaManager.ReserveTokens("alice", 40000)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	if quotaDecision = quotaManager.CheckAndCount("alice"); quotaDecision.Code != "user_token_limit" || quotaManager.globalRequests != 0 {
		testContext.Fatal(quotaDecision)
	}
	if err := quotaManager.SettleTokens(reservation, 10000); err != nil {
		testContext.Fatal(err)
	}
	if err := quotaManager.SettleTokens(reservation, 0); err == nil {
		testContext.Fatal("double settlement accepted")
	}
	if available(quotaManager.budgets["alice"], 40000) != 30000 || !quotaManager.CheckAndCount("alice").Allowed {
		testContext.Fatal("unused hold not released")
	}
	old, quotaDecision := quotaManager.ReserveTokens("alice", 30000)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	*now = now.Add(time.Hour)
	current, quotaDecision := quotaManager.ReserveTokens("alice", 40000)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	if err := quotaManager.SettleTokens(old, 20000); err != nil {
		testContext.Fatal(err)
	}
	if available(quotaManager.budgets["alice"], 40000) != 0 {
		testContext.Fatal("old settlement changed new window")
	}
	if err := quotaManager.SettleTokens(current, 0); err != nil {
		testContext.Fatal(err)
	}
	if !quotaManager.CheckAndCount("alice").Allowed {
		testContext.Fatal("release failed")
	}
}

func TestGlobalTokenExhaustionAndAtomicReserve(testContext *testing.T) {
	quotaManager, _ := testManager(testContext, DefaultConfig())
	for _, user := range []string{"a", "b"} {
		reservation, quotaDecision := quotaManager.ReserveTokens(user, 40000)
		if !quotaDecision.Allowed {
			testContext.Fatal(quotaDecision)
		}
		if err := quotaManager.SettleTokens(reservation, 40000); err != nil {
			testContext.Fatal(err)
		}
	}
	if _, quotaDecision := quotaManager.ReserveTokens("c", 30000); quotaDecision.Code != "global_token_limit" {
		testContext.Fatal(quotaDecision)
	}
	if quotaManager.budgets["c"] != nil || quotaManager.globalBudget.reserved != 0 {
		testContext.Fatal("partial reservation")
	}
	_, quotaDecision := quotaManager.ReserveTokens("c", 20000)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	if quotaDecision = quotaManager.CheckAndCount("d"); quotaDecision.Code != "global_token_limit" || quotaManager.globalRequests != 0 {
		testContext.Fatal(quotaDecision)
	}
}

func TestConcurrentReservations(testContext *testing.T) {
	quotaManager, _ := testManager(testContext, DefaultConfig())
	var count atomic.Int64
	var wg sync.WaitGroup
	for index := 0; index < 200; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, quotaDecision := quotaManager.ReserveTokens(string(rune(1000+index)), 1000)
			if quotaDecision.Allowed {
				count.Add(1)
			}
		}(index)
	}
	wg.Wait()
	if count.Load() != 100 || quotaManager.globalBudget.reserved != 100000 {
		testContext.Fatal("oversubscribed tokens")
	}
}
