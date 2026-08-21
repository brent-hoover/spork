// Package specverify is the avspec verifier seam: avspec verify <dir> --json, run as a subprocess.
//
// NOT a declared avspec module. avspec is a Python tool, so the integration
// ring needs a uv environment as well as Go.
//
// A refusal is not an error: the verifier exits 1 whenever ok=false, which is
// the ordinary outcome for a spec kriya must decline. A non-zero exit with a
// parseable payload returns a Report, never an error.
package specverify
