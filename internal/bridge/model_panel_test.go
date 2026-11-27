package bridge

import "testing"

func TestPanelModelChangeKeepsRoutingPolicy(t *testing.T) {
	for _, triage := range []string{"", "dedicated-triage"} {
		e := &Engine{}
		if err := e.SetModelRouting(ModelRoutingConfig{Enabled: true, SmartModel: "smart", TriageModel: triage}, "old-fast"); err != nil {
			t.Fatal(err)
		}
		previous := e.modelRouter.Load()
		e.SetFastModel("new-fast")
		next := e.modelRouter.Load()
		if previous.fastModel != "old-fast" || next.fastModel != "new-fast" || next.cfg.SmartModel != "smart" {
			t.Fatal("panel change lost routing policy or mutated in-flight snapshot")
		}
		wantTriage := triage
		if triage == "" {
			wantTriage = "new-fast"
		}
		if next.triageModel != wantTriage {
			t.Fatalf("triage = %q", next.triageModel)
		}
		model, _, _ := next.Select(t.Context(), "привет")
		if model != "new-fast" {
			t.Fatalf("fast = %q", model)
		}
		model, _, _ = next.Select(t.Context(), "сравни варианты")
		if model != "smart" {
			t.Fatalf("smart = %q", model)
		}
	}
}
