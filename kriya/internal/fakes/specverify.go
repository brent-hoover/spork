package fakes

import (
	"context"
	"errors"

	"kriya/internal/specverify"
)

// Verifier is a programmable specverify.Verifier.
//
// Reports is keyed by directory so one fake can serve a test that verifies
// several specs. Err, when set, is returned for every directory — the seam
// distinguishes a refusal (a Report with OK false) from a malfunction (an
// error), and tests need to drive both.
type Verifier struct {
	Reports map[string]specverify.Report
	Models  map[string]specverify.Model
	Err     error
	// Calls records the directories asked about, in order, so a test can
	// assert that intake verified what it claimed to.
	Calls []string
}

// NewVerifier returns a Verifier that reports r for dir.
func NewVerifier(dir string, r specverify.Report) *Verifier {
	return &Verifier{Reports: map[string]specverify.Report{dir: r}}
}

// Verify returns the programmed report for dir.
func (v *Verifier) Verify(_ context.Context, dir string) (specverify.Report, error) {
	v.Calls = append(v.Calls, dir)
	if v.Err != nil {
		return specverify.Report{}, v.Err
	}
	r, ok := v.Reports[dir]
	if !ok {
		return specverify.Report{}, errors.New("fakes: no report programmed for " + dir)
	}
	return r, nil
}

// Resolve returns the programmed model for dir.
func (v *Verifier) Resolve(_ context.Context, dir string) (specverify.Model, error) {
	v.Calls = append(v.Calls, dir)
	if v.Err != nil {
		return specverify.Model{}, v.Err
	}
	m, ok := v.Models[dir]
	if !ok {
		return specverify.Model{}, errors.New("fakes: no model programmed for " + dir)
	}
	return m, nil
}

// WithModules programs a model whose modules carry cmds, and returns v so it
// can be chained onto NewVerifier.
func (v *Verifier) WithModules(dir string, mods ...specverify.Module) *Verifier {
	if v.Models == nil {
		v.Models = map[string]specverify.Model{}
	}
	v.Models[dir] = specverify.Model{OK: true, Modules: mods}
	return v
}
