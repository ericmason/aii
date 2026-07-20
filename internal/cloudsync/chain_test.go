package cloudsync

import (
	"testing"

	"github.com/ericmason/aii/internal/store"
)

func msgs(contents ...string) []store.Row {
	out := make([]store.Row, len(contents))
	for i, c := range contents {
		out[i] = store.Row{Ordinal: i, Role: "user", TS: int64(1700000000 + i), Content: c}
	}
	return out
}

func TestChainIncrementalEqualsFromScratch(t *testing.T) {
	m := msgs("a", "b", "c", "d")
	full := computeChain("claude_code", "uid-1", m)

	inc := chainSeed("claude_code", "uid-1")
	for _, r := range m {
		inc = chainNext(inc, r.Role, r.TS, r.Content)
	}
	if full != inc {
		t.Fatal("incremental chain != from-scratch chain")
	}
	if chainAt("claude_code", "uid-1", m, 4) != full {
		t.Fatal("chainAt(len) != full chain")
	}
}

func TestChainDetectsDivergence(t *testing.T) {
	a := computeChain("claude_code", "uid-1", msgs("a", "b", "c"))
	b := computeChain("claude_code", "uid-1", msgs("a", "b", "X"))
	if a == b {
		t.Fatal("divergent content produced equal chains")
	}
	// Same content, different session identity → different chains.
	c := computeChain("claude_code", "uid-2", msgs("a", "b", "c"))
	if a == c {
		t.Fatal("different sessions produced equal chains")
	}
	// Role and ts are part of the chain.
	m := msgs("a")
	m[0].Role = "assistant"
	if computeChain("claude_code", "uid-1", m) == computeChain("claude_code", "uid-1", msgs("a")) {
		t.Fatal("role not folded into chain")
	}
}

func TestChainPrefixProperty(t *testing.T) {
	longer := msgs("a", "b", "c", "d", "e")
	shorter := msgs("a", "b", "c")
	// The chain at the shorter length over the longer history must
	// equal the shorter history's head — that's the prefix check
	// push/pull rely on.
	if chainAt("cc", "u", longer, 3) != computeChain("cc", "u", shorter) {
		t.Fatal("prefix property violated")
	}
	divergent := msgs("a", "X", "c")
	if chainAt("cc", "u", longer, 3) == computeChain("cc", "u", divergent) {
		t.Fatal("divergent prefix not detected")
	}
}
