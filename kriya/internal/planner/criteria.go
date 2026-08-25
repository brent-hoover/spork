package planner

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// criterionID matches the id prefixes avspec pins: REQ- and AC-.
var criterionID = regexp.MustCompile(`\b(REQ|AC)-[A-Za-z0-9-]+`)

// knownCriteria collects every REQ and AC id in the pinned manifest.
//
// Read from the SNAPSHOT, never the working tree: a ticket's citations are
// checked against the spec that was validated, so an edit made after intake
// cannot retroactively make a bad citation look valid.
func knownCriteria(snap Snapshot) map[string]bool {
	out := map[string]bool{}
	for path, content := range snap.Content {
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			continue
		}
		for _, id := range criterionID.FindAllString(content, -1) {
			out[id] = true
		}
	}
	return out
}

// decomposePrompt asks the PM for tracer bullets.
//
// The manifest travels as content rather than a path: the agent must decompose
// the SNAPSHOT, and a path would let it read the working tree, which may
// already differ from what intake validated.
func decomposePrompt(snap Snapshot, known map[string]bool) string {
	ids := make([]string, 0, len(known))
	for id := range known {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	b.WriteString("Decompose this specification into tracer-bullet tickets.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Every implementation ticket is a thin end-to-end slice, not a layer. Name the layers it cuts\n")
	b.WriteString("  through in \"layers\" — at least two, or it is a stripe of one layer and will be rejected.\n")
	b.WriteString("- Mark exactly one implementation ticket \"skeleton\": true. It is the walking skeleton, the\n")
	b.WriteString("  thinnest slice that touches EVERY layer any other ticket touches, and it is worked first.\n")
	b.WriteString("- Spike and research tickets are exempt: they cut through no layers and are never the skeleton.\n")
	b.WriteString("- A spike may gate the skeleton ONLY if it gates every other implementation ticket too.\n")
	b.WriteString("  Otherwise leave the skeleton ungated: an unblocked ticket would be worked ahead of it.\n")
	b.WriteString("- Every ticket cites the REQ and AC ids it satisfies as BARE IDS, never prose, and may cite ONLY these:\n  ")
	b.WriteString(strings.Join(ids, ", "))
	b.WriteString("\n\nSpecification:\n")
	for _, path := range sortedKeys(snap.Content) {
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			continue
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", path, snap.Content[path])
	}
	return b.String()
}
