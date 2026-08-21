// Package recovery sequences each owning module's Recover across the nine reconciliation
// stages, at startup, before any pop.
//
// NOT a declared avspec module. A legal route exists through devloop, which
// may import reviewbridge — but it would put cross-module startup sequencing
// inside a domain module, and inside the one module
// CON-deterministic-orchestrator most constrains. Sequencing is a
// composition-root concern. See feature-work/kriya-build/spec-gaps.md.
//
// Imported only by cmd/kriya. No module imports it.
package recovery
