package planner_test

import (
	"context"

	"kriya/internal/planner"
)

// memAttempts is an in-memory AttemptStore.
//
// Here rather than in internal/fakes because AttemptStore is planner's OWN
// port, not a seam: fakes doubles the seams several packages share, and
// arch-go forbids it importing any module.
type memAttempts struct {
	byToken map[string]planner.IntakeAttempt
	next    map[string]int
	mapping map[string]planner.SpecMapping
}

func newMemAttempts() *memAttempts {
	return &memAttempts{
		byToken: map[string]planner.IntakeAttempt{},
		next:    map[string]int{},
		mapping: map[string]planner.SpecMapping{},
	}
}

func (m *memAttempts) Reserve(_ context.Context, token, targetKey string) (planner.IntakeAttempt, error) {
	if a, ok := m.byToken[token]; ok {
		return a, nil
	}
	m.next[targetKey]++
	a := planner.IntakeAttempt{
		Token: token, TargetKey: targetKey,
		Generation: m.next[targetKey], State: planner.AttemptPending,
	}
	m.byToken[token] = a
	return a, nil
}

func (m *memAttempts) Complete(_ context.Context, token, specHash string) error {
	a := m.byToken[token]
	a.SpecHash = specHash
	a.State = planner.AttemptComplete
	m.byToken[token] = a
	return nil
}

func (m *memAttempts) MapSpec(_ context.Context, s planner.SpecMapping) error {
	if cur, ok := m.mapping[s.TargetKey]; ok && cur.Generation >= s.Generation {
		return nil
	}
	m.mapping[s.TargetKey] = s
	return nil
}

func (m *memAttempts) Mapping(_ context.Context, targetKey string) (planner.SpecMapping, bool, error) {
	s, ok := m.mapping[targetKey]
	return s, ok, nil
}
