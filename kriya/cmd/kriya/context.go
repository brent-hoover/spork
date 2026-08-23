package main

import (
	kctx "kriya/internal/context"
	"kriya/internal/specverify"
)

// specForContext maps a pinned snapshot onto what the context module needs.
//
// The mapping lives in the composition root because MOD-context's declared
// boundary permits only the tracker client — it may not import the planner or
// the spec verifier. Being explicit is the point: this function is where "what
// the agent gets to see" is decided, and it reads as one thing rather than
// being spread across two packages.
func specForContext(
	law []specverify.Module,
	constitution []specverify.ConstitutionEntry,
	artifacts map[string]string,
) kctx.Spec {
	spec := kctx.Spec{Artifacts: artifacts}
	for _, entry := range constitution {
		spec.Constitution = append(spec.Constitution,
			kctx.Principle{ID: entry.ID, Statement: entry.Statement})
	}
	for _, module := range law {
		m := kctx.Module{
			ID: module.ID, Name: module.Name,
			MayImport: module.MayImport, Commands: module.Commands,
		}
		for _, c := range module.Contracts {
			m.Contracts = append(m.Contracts,
				kctx.Contract{ID: c.ID, Type: c.Type, Path: c.Path})
		}
		spec.Modules = append(spec.Modules, m)
	}
	return spec
}
