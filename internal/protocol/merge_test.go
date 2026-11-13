package protocol

import "testing"

func TestMergeRequiresCIAndAnExplicitMethod(t *testing.T) {
	const base = "name: merge\nroster: {}\nphases:\n  - {name: test, kind: code, command: 'true'}\n"
	for _, method := range []string{"squash", "merge", "rebase"} {
		spec := mustParse(t, base+"publish: {ci: {wait: true}, merge: {method: "+method+"}}\n")
		if !spec.Merges() {
			t.Fatal("merge opt-in lost")
		}
	}
	for _, policy := range []string{"merge: {method: squash}", "ci: {wait: false}, merge: {method: squash}", "ci: {wait: true}, merge: {}", "ci: {wait: true}, merge: {method: auto}"} {
		mustReject(t, base+"publish: {"+policy+"}\n", "publish: merge")
	}
	if mustParse(t, base).Merges() {
		t.Fatal("merge must be opt-in")
	}
}
