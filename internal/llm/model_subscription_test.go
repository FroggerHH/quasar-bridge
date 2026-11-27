package llm

import (
	"encoding/json/v2"
	"net/http"
	"testing"
)

func TestSubscriptionPanelModelAndRequestOverride(t *testing.T) {
	selected := "panel-fast"
	var seen []string
	c := chatGPTFixtureClient(t, func(r *http.Request) string {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, body.Model)
		return completedEvent(responseAnswer)
	})
	c.SubscriptionModel = func() string { return selected }
	for i, model := range []string{"", "smart", ""} {
		if i == 2 {
			selected = "new-panel-fast"
		}
		if _, err := c.Complete(WithModel(t.Context(), model), []Message{{Role: "user", Content: "fixture"}}, false); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []string{"panel-fast", "smart", "new-panel-fast"} {
		if seen[i] != want {
			t.Fatalf("models = %v", seen)
		}
	}
}
