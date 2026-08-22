package planner_test

import "strings"

func splitOn(s string, sep rune) []string { return strings.Split(s, string(sep)) }
func trimSpace(s string) string           { return strings.TrimSpace(s) }
