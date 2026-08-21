// Package main is the composition root: opens the store, applies each module's DDL, wires the
// modules together, runs recovery, and owns main().
//
// NOT a declared avspec module — avspec has no composition-root concept, yet
// arch-go requires 100%% package coverage. Same gap sutra logged.
package main
