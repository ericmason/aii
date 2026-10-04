package cloudsync

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/ericmason/aii/internal/store"
)

// The consistency model: every session carries a rolling hash chain
// over its messages. The chain head after N messages identifies the
// exact content prefix, so push/pull can prove "remote is a strict
// prefix of local" (safe to extend) versus "histories diverged"
// (never merge — resolve by epoch bump, higher epoch supersedes).
// Raw message counts are never trusted: they collide under transcript
// rotation, renumbering, and divergent redaction.
//
//	c_0 = SHA256("aii-chain/v1" || agent || 0x00 || uid)
//	c_i = SHA256(c_{i-1} || role || 0x00 || be64(ts) || 0x00 || content)

type chainHash [32]byte

func chainSeed(agent, uid string) chainHash {
	h := sha256.New()
	h.Write([]byte("aii-chain/v1"))
	h.Write([]byte(agent))
	h.Write([]byte{0})
	h.Write([]byte(uid))
	var out chainHash
	copy(out[:], h.Sum(nil))
	return out
}

func chainNext(prev chainHash, role string, ts int64, content string) chainHash {
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(ts))
	h := sha256.New()
	h.Write(prev[:])
	h.Write([]byte(role))
	h.Write([]byte{0})
	h.Write(tsBuf[:])
	h.Write([]byte{0})
	h.Write([]byte(content))
	var out chainHash
	copy(out[:], h.Sum(nil))
	return out
}

// chainOver extends a chain over messages in the order given.
func chainOver(c chainHash, msgs []store.Row) chainHash {
	for _, m := range msgs {
		c = chainNext(c, m.Role, m.TS, m.Content)
	}
	return c
}

// computeChain returns the chain head over the full message sequence.
func computeChain(agent, uid string, msgs []store.Row) chainHash {
	return chainOver(chainSeed(agent, uid), msgs)
}

// chainAt returns the chain head after the first n messages.
func chainAt(agent, uid string, msgs []store.Row, n int) chainHash {
	if n > len(msgs) {
		n = len(msgs)
	}
	return chainOver(chainSeed(agent, uid), msgs[:n])
}
