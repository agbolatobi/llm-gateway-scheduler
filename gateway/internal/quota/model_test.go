package quota

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestThreeAccountReservation(testContext *testing.T) {
	config := DefaultConfig()
	config.Models[0].Limits.Tokens = 100
	config.User.Tokens = 1000
	quotaManager, _ := testManager(testContext, config)
	reservation, quotaDecision := quotaManager.ReserveForModel("alice", "local-llm", 70)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	if _, quotaDecision := quotaManager.ReserveForModel("bob", "local-llm", 40); quotaDecision.Code != "model_token_limit" {
		testContext.Fatal(quotaDecision)
	}
	if quotaManager.budgets["bob"] != nil || quotaManager.globalBudget.reserved != 70 || quotaManager.modelRequests["local-llm"] != 1 {
		testContext.Fatal("partial charge")
	}
	if err := quotaManager.SettleTokens(reservation, 20); err != nil {
		testContext.Fatal(err)
	}
	if quotaManager.modelBudgets["local-llm"].spent != 20 || quotaManager.modelBudgets["local-llm"].reserved != 0 {
		testContext.Fatal("model settlement")
	}
	if _, quotaDecision := quotaManager.ReserveForModel("bob", "default", 1); quotaDecision.Code != "model_not_found" {
		testContext.Fatal("manager must only accept canonical IDs")
	}
}

func TestModelRequestLimitAndRemoval(testContext *testing.T) {
	config := DefaultConfig()
	config.Models[0].Limits.Requests = 1
	config.Models = append(config.Models, Model{ID: "second", Limits: Limits{30, 100000}})
	quotaManager, _ := testManager(testContext, config)
	reservation, quotaDecision := quotaManager.ReserveForModel("alice", "local-llm", 1)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	quotaManager.SettleTokens(reservation, 0)
	if _, quotaDecision := quotaManager.ReserveForModel("bob", "local-llm", 1); quotaDecision.Code != "model_request_limit" {
		testContext.Fatal(quotaDecision)
	}
	removed := config
	removed.Models = config.Models[1:]
	if err := quotaManager.Reconfigure(removed); err != nil {
		testContext.Fatal(err)
	}
	if err := quotaManager.Reconfigure(config); err != nil {
		testContext.Fatal(err)
	}
	if _, quotaDecision := quotaManager.ReserveForModel("bob", "local-llm", 1); quotaDecision.Code != "model_request_limit" {
		testContext.Fatal("re-registration reset usage")
	}
}

func TestConcurrentModelReservations(testContext *testing.T) {
	config := DefaultConfig()
	config.Models[0].Limits.Requests = 10
	quotaManager, _ := testManager(testContext, config)
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for index := 0; index < 100; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, quotaDecision := quotaManager.ReserveForModel("alice", "local-llm", 1); quotaDecision.Allowed {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 10 || quotaManager.globalBudget.reserved != 10 || quotaManager.modelBudgets["local-llm"].reserved != 10 {
		testContext.Fatal("oversubscription")
	}
}
