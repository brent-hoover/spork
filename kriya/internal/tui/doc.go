// Package tui is the the operator dashboard — live overview of running and queued work; where
// agents surface questions, escalations, and the review bridge's persisted
// orphan watch.
//
// Holds no second source of truth: it reads the same durable records
// recovery reads, reaching planner- and workspace-owned state through the
// orchestrator's read facade.
package tui
