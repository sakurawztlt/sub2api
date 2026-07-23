package openai

import "testing"

func TestDefaultModelsPreferConcreteGPT56Sol(t *testing.T) {
	if len(DefaultModels) < 2 {
		t.Fatalf("expected GPT-5.6 models in default catalog")
	}
	if got := DefaultModels[0].ID; got != "gpt-5.6-sol" {
		t.Fatalf("first default model = %q, want gpt-5.6-sol", got)
	}
	if got := DefaultModels[1].ID; got != "gpt-5.6" {
		t.Fatalf("second default model = %q, want gpt-5.6 alias", got)
	}
}
