// Package context handles context assembly per ticket and the learning store.
//
// Owns Learning and ContextBundle. Also writes the per-ticket gate wrapper
// scripts the dev agent is permitted to run.
//
// NOTE: this package is named context, mirroring MOD-context, and therefore
// shadows the standard library's context package. Callers needing both must
// alias, e.g. kctx "kriya/internal/context".
package context
