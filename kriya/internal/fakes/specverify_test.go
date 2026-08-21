package fakes_test

import (
	"context"
	"testing"

	"kriya/internal/fakes"
	"kriya/internal/specverify"
)

func TestVerifierReturnsTheProgrammedReport(t *testing.T) {
	want := specverify.Report{Status: "ready", OK: true}
	var v specverify.Verifier = fakes.NewVerifier("/spec", want)
	got, err := v.Verify(context.Background(), "/spec")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Status != want.Status || got.OK != want.OK {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestVerifierRecordsWhatItWasAsked(t *testing.T) {
	v := fakes.NewVerifier("/a", specverify.Report{Status: "ready", OK: true})
	v.Reports["/b"] = specverify.Report{Status: "draft", OK: true}
	for _, dir := range []string{"/a", "/b", "/a"} {
		if _, err := v.Verify(context.Background(), dir); err != nil {
			t.Fatalf("verify %s: %v", dir, err)
		}
	}
	want := []string{"/a", "/b", "/a"}
	if len(v.Calls) != len(want) {
		t.Fatalf("recorded %v, want %v", v.Calls, want)
	}
	for i := range want {
		if v.Calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, v.Calls[i], want[i])
		}
	}
}

func TestVerifierCanReportAMalfunction(t *testing.T) {
	v := fakes.NewVerifier("/spec", specverify.Report{})
	v.Err = context.DeadlineExceeded
	if _, err := v.Verify(context.Background(), "/spec"); err == nil {
		t.Fatal("expected the programmed error")
	}
}

func TestVerifierErrsWhenNothingIsProgrammed(t *testing.T) {
	v := &fakes.Verifier{}
	if _, err := v.Verify(context.Background(), "/unknown"); err == nil {
		t.Fatal("an unprogrammed directory must error, not report a zero value")
	}
}
