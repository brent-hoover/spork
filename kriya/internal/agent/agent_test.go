package agent_test

import (
	"testing"

	"kriya/internal/agent"
)

func tiers() agent.Tiers {
	return agent.Tiers{
		Roles:  map[agent.Role]string{agent.RolePM: "big", agent.RoleDev: "small"},
		Models: map[string]string{"big": "model-b", "small": "model-s"},
	}
}

func TestARoleResolvesToItsConfiguredModel(t *testing.T) {
	tier, model, err := tiers().Resolve(agent.RolePM)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tier != "big" || model != "model-b" {
		t.Errorf("got tier=%q model=%q, want big/model-b", tier, model)
	}
}

func TestAMissingTierFailsLoudly(t *testing.T) {
	// AC-tier-explicit: there is no silent default. A role that falls through
	// to some built-in model is how a build runs for a week on the wrong one.
	if _, _, err := tiers().Resolve(agent.RoleSA); err == nil {
		t.Fatal("an unconfigured role must error")
	}
}

func TestATierNamingNoModelFailsLoudly(t *testing.T) {
	tr := tiers()
	tr.Roles[agent.RoleSA] = "nonexistent"
	if _, _, err := tr.Resolve(agent.RoleSA); err == nil {
		t.Fatal("a tier with no model must error")
	}
}

func TestValidateReportsTheFirstUnconfiguredRole(t *testing.T) {
	// Called at startup so the failure lands before any build begins.
	if err := tiers().Validate(agent.RolePM, agent.RoleDev); err != nil {
		t.Fatalf("configured roles should validate: %v", err)
	}
	if err := tiers().Validate(agent.RolePM, agent.RolePO); err == nil {
		t.Fatal("an unconfigured role must fail validation")
	}
	if err := (agent.Tiers{}).Validate(agent.RolePM); err == nil {
		t.Fatal("empty configuration must fail validation")
	}
}
