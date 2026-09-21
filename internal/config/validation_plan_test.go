package config

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEffectiveRepoConfig_ValidationPlanTrustedOnly(t *testing.T) {
	trusted := &RepoConfig{
		ValidationPlan: &ValidationPlanRaw{
			Version:        "v1",
			PlanID:         "trusted-plan",
			SelectedInputs: []string{"**/*.go", "go.mod"},
			RequiredPhases: []string{"review", "test", "ci"},
			PhaseWriteSets: map[string][]string{
				"document": {"docs/**"},
			},
		},
	}

	// Pushed branch attempts to weaken requirements: only review, unrestricted writes
	pushed := &RepoConfig{
		ValidationPlan: &ValidationPlanRaw{
			Version:        "v1",
			PlanID:         "pushed-weakened-plan",
			SelectedInputs: []string{"none"},
			RequiredPhases: []string{"review"},
			PhaseWriteSets: map[string][]string{
				"review":   {"**/*"},
				"document": {"**/*"},
				"lint":     {"**/*"},
			},
		},
	}

	// Case 1: allowRepoCommands = false
	effective := EffectiveRepoConfig(pushed, trusted, false)
	if effective.ValidationPlan == nil || effective.ValidationPlan.PlanID != "trusted-plan" {
		t.Fatalf("expected trusted plan to be preserved, got %+v", effective.ValidationPlan)
	}

	// Case 2: allowRepoCommands = true - still trusted-only!
	effectiveCommandsOptIn := EffectiveRepoConfig(pushed, trusted, true)
	if effectiveCommandsOptIn.ValidationPlan == nil || effectiveCommandsOptIn.ValidationPlan.PlanID != "trusted-plan" {
		t.Fatalf("expected trusted plan to be preserved even under allowRepoCommands, got %+v", effectiveCommandsOptIn.ValidationPlan)
	}

	// Case 3: trusted is nil
	effectiveNoTrusted := EffectiveRepoConfig(pushed, nil, false)
	if effectiveNoTrusted.ValidationPlan != nil {
		t.Fatalf("expected nil validation plan when trusted copy is nil, got %+v", effectiveNoTrusted.ValidationPlan)
	}
}

func TestMerge_ValidationPlanConservativeDefault(t *testing.T) {
	global := &GlobalConfig{}
	repo := &RepoConfig{} // No validation plan declared

	cfg := Merge(global, repo)
	if cfg.ValidationPlan.PlanID != "conservative-default" {
		t.Fatalf("expected conservative-default plan ID, got %s", cfg.ValidationPlan.PlanID)
	}
	if !cfg.ValidationPlan.ConservativeDefault {
		t.Fatal("expected ConservativeDefault to be true")
	}
	if len(cfg.ValidationPlan.SelectedInputs) == 0 || cfg.ValidationPlan.SelectedInputs[0] != "**/*" {
		t.Fatalf("expected full-tree selected inputs, got %+v", cfg.ValidationPlan.SelectedInputs)
	}
	if cfg.ValidationPlan.PlanDigest == "" {
		t.Fatal("expected non-empty plan digest")
	}
}

func TestMerge_ValidationPlanCustom(t *testing.T) {
	global := &GlobalConfig{}
	repo := &RepoConfig{
		ValidationPlan: &ValidationPlanRaw{
			PlanID:         "custom-plan",
			SelectedInputs: []string{"cmd/**", "internal/**"},
			RequiredPhases: []string{"review", "test"},
		},
	}

	cfg := Merge(global, repo)
	if cfg.ValidationPlan.PlanID != "custom-plan" {
		t.Fatalf("expected custom-plan, got %s", cfg.ValidationPlan.PlanID)
	}
	if cfg.ValidationPlan.ConservativeDefault {
		t.Fatal("expected ConservativeDefault to be false for custom plan")
	}
	if len(cfg.ValidationPlan.SelectedInputs) != 2 {
		t.Fatalf("expected 2 selected inputs, got %+v", cfg.ValidationPlan.SelectedInputs)
	}
	if len(cfg.ValidationPlan.RequiredPhases) != 2 || cfg.ValidationPlan.RequiredPhases[0] != types.StepReview {
		t.Fatalf("expected review and test required phases, got %+v", cfg.ValidationPlan.RequiredPhases)
	}
}
