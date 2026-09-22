package config

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func cloneValidationPlan(raw *ValidationPlanRaw) *ValidationPlanRaw {
	if raw == nil {
		return nil
	}
	clone := *raw
	clone.SelectedInputs = append([]string(nil), raw.SelectedInputs...)
	clone.RequiredPhases = append([]string(nil), raw.RequiredPhases...)
	clone.ProtectedExclusions = append([]string(nil), raw.ProtectedExclusions...)
	if raw.Commands != nil {
		clone.Commands = make(map[string]string)
		for k, v := range raw.Commands {
			clone.Commands[k] = v
		}
	}
	if raw.DependencyEdges != nil {
		clone.DependencyEdges = make(map[string][]string)
		for k, v := range raw.DependencyEdges {
			clone.DependencyEdges[k] = append([]string(nil), v...)
		}
	}
	if raw.PhaseWriteSets != nil {
		clone.PhaseWriteSets = make(map[string][]string)
		for k, v := range raw.PhaseWriteSets {
			clone.PhaseWriteSets[k] = append([]string(nil), v...)
		}
	}
	return &clone
}

// ToValidationPlan converts raw repo validation plan configuration to a validated ValidationPlan.
func (r *ValidationPlanRaw) ToValidationPlan(cfg *Config) types.ValidationPlan {
	if r == nil {
		return DefaultConservativeValidationPlan(cfg)
	}
	version := strings.TrimSpace(r.Version)
	if version == "" {
		version = types.WorkGenerationProtocolVersion
	}
	planID := strings.TrimSpace(r.PlanID)
	if planID == "" {
		planID = "repo-plan"
	}
	selected := append([]string(nil), r.SelectedInputs...)
	if len(selected) == 0 {
		selected = []string{"**/*"}
	}
	var phases []types.StepName
	for _, p := range r.RequiredPhases {
		p = strings.TrimSpace(p)
		if p != "" {
			phases = append(phases, types.StepName(p))
		}
	}
	if len(phases) == 0 {
		phases = []types.StepName{
			types.StepReview,
			types.StepTest,
			types.StepDocument,
			types.StepLint,
			types.StepPush,
			types.StepPR,
			types.StepCI,
		}
	}
	cmds := make(map[string]string)
	for k, v := range r.Commands {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			cmds[k] = v
		}
	}
	if cfg != nil {
		if _, ok := cmds["test"]; !ok && cfg.Commands.Test != "" {
			cmds["test"] = cfg.Commands.Test
		}
		if _, ok := cmds["lint"]; !ok && cfg.Commands.Lint != "" {
			cmds["lint"] = cfg.Commands.Lint
		}
	}
	edges := make(map[string][]string)
	for k, v := range r.DependencyEdges {
		edges[k] = append([]string(nil), v...)
	}
	writeSets := make(map[string][]string)
	for k, v := range r.PhaseWriteSets {
		writeSets[k] = append([]string(nil), v...)
	}
	if _, ok := writeSets["rebase"]; !ok {
		writeSets["rebase"] = []string{"**/*"}
	}
	if _, ok := writeSets["document"]; !ok {
		writeSets["document"] = []string{"docs/**", "*.md", "README*"}
	}
	if _, ok := writeSets["review"]; !ok {
		writeSets["review"] = []string{}
	}
	if _, ok := writeSets["review_fix"]; !ok {
		writeSets["review_fix"] = []string{"**/*"}
	}
	if _, ok := writeSets["lint"]; !ok {
		writeSets["lint"] = []string{}
	}
	exclusions := defaultProtectedExclusions()
	if len(r.ProtectedExclusions) > 0 {
		exclusions = append(exclusions, r.ProtectedExclusions...)
	}
	if cfg != nil && len(cfg.ProtectedPaths) > 0 {
		exclusions = append(exclusions, cfg.ProtectedPaths...)
	}

	plan := types.ValidationPlan{
		Version:             version,
		PlanID:              planID,
		SelectedInputs:      selected,
		RequiredPhases:      phases,
		Commands:            cmds,
		DependencyEdges:     edges,
		PhaseWriteSets:      writeSets,
		ProtectedExclusions: exclusions,
		ConservativeDefault: false,
	}
	plan.PlanDigest = types.ComputePlanDigest(&plan)
	return plan
}

// DefaultConservativeValidationPlan generates the conservative full-tree/check-only
// default plan when no explicit plan is provided in trusted repo config.
func DefaultConservativeValidationPlan(cfg *Config) types.ValidationPlan {
	selected := []string{"**/*"}
	required := []types.StepName{
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}
	cmds := make(map[string]string)
	if cfg != nil {
		if cfg.Commands.Test != "" {
			cmds["test"] = cfg.Commands.Test
		}
		if cfg.Commands.Lint != "" {
			cmds["lint"] = cfg.Commands.Lint
		}
	}
	writeSets := map[string][]string{
		"rebase":     {"**/*"},
		"review":     {},
		"review_fix": {"**/*"},
		"test":       {},
		"document":   {"docs/**", "*.md", "README*"},
		"lint":       {},
		"push":       {},
		"pr":         {},
		"ci":         {},
	}
	if cfg != nil && cfg.Commands.Lint != "" {
		writeSets["lint_fix"] = defaultLintFixWriteSet()
	}
	exclusions := defaultProtectedExclusions()
	if cfg != nil && len(cfg.ProtectedPaths) > 0 {
		exclusions = append(exclusions, cfg.ProtectedPaths...)
	}

	plan := types.ValidationPlan{
		Version:             types.WorkGenerationProtocolVersion,
		PlanID:              "conservative-default",
		SelectedInputs:      selected,
		RequiredPhases:      required,
		Commands:            cmds,
		PhaseWriteSets:      writeSets,
		ProtectedExclusions: exclusions,
		ConservativeDefault: true,
		Reason:              "default conservative full-tree/check-only plan",
	}
	plan.PlanDigest = types.ComputePlanDigest(&plan)
	return plan
}

func defaultLintFixWriteSet() []string {
	return []string{
		"*.go",
		"**/*.go",
		"*.js",
		"**/*.js",
		"*.ts",
		"**/*.ts",
		"*.tsx",
		"**/*.tsx",
		"*.jsx",
		"**/*.jsx",
		"*.py",
		"**/*.py",
		"*.rs",
		"**/*.rs",
		"*.java",
		"**/*.java",
		"*.c",
		"**/*.c",
		"*.cc",
		"**/*.cc",
		"*.cpp",
		"**/*.cpp",
		"*.h",
		"**/*.h",
		"*.hpp",
		"**/*.hpp",
	}
}

func defaultProtectedExclusions() []string {
	return []string{
		".git",
		".git/**",
		".no-mistakes.yaml",
		".no-mistakes/**",
		"**/*.pem",
		"**/*.key",
		"**/*token*",
		"**/*secret*",
		"**/*credential*",
	}
}
