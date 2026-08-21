// Package workspace manages the worktree/branch lifecycle per ticket.
//
// Owns Workspace. Git access is by shelling out, behind an interface, so the
// unit ring can fake it.
package workspace
