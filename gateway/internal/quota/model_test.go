package quota

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestThreeAccountReservation(t *testing.T) {
	c := DefaultConfig()
	c.Models[0].Limits.Tokens = 100
	c.User.Tokens = 1000
	m, _ := testManager(t, c)
	r, d := m.ReserveForModel("alice", "local-llm", 70)
	if !d.Allowed {
		t.Fatal(d)
	}
	if _, d := m.ReserveForModel("bob", "local-llm", 40); d.Code != "model_token_limit" {
		t.Fatal(d)
	}
	if m.budgets["bob"] != nil || m.globalBudget.reserved != 70 || m.modelRequests["local-llm"] != 1 {
		t.Fatal("partial charge")
	}
	if err := m.SettleTokens(r, 20); err != nil {
		t.Fatal(err)
	}
	if m.modelBudgets["local-llm"].spent != 20 || m.modelBudgets["local-llm"].reserved != 0 {
		t.Fatal("model settlement")
	}
	if _, d := m.ReserveForModel("bob", "default", 1); d.Code != "model_not_found" {
		t.Fatal("manager must only accept canonical IDs")
	}
}

func TestModelRequestLimitAndRemoval(t *testing.T) {
	c := DefaultConfig()
	c.Models[0].Limits.Requests = 1
	c.Models = append(c.Models, Model{ID: "second", Limits: Limits{30, 100000}})
	m, _ := testManager(t, c)
	r, d := m.ReserveForModel("alice", "local-llm", 1)
	if !d.Allowed {
		t.Fatal(d)
	}
	m.SettleTokens(r, 0)
	if _, d := m.ReserveForModel("bob", "local-llm", 1); d.Code != "model_request_limit" {
		t.Fatal(d)
	}
	removed := c
	removed.Models = c.Models[1:]
	if err := m.Reconfigure(removed); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconfigure(c); err != nil {
		t.Fatal(err)
	}
	if _, d := m.ReserveForModel("bob", "local-llm", 1); d.Code != "model_request_limit" {
		t.Fatal("re-registration reset usage")
	}
}

func TestConcurrentModelReservations(t *testing.T) {
	c := DefaultConfig()
	c.Models[0].Limits.Requests = 10
	m, _ := testManager(t, c)
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, d := m.ReserveForModel("alice", "local-llm", 1); d.Allowed {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 10 || m.globalBudget.reserved != 10 || m.modelBudgets["local-llm"].reserved != 10 {
		t.Fatal("oversubscription")
	}
}
