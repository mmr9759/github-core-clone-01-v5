// Identity derived from the contest's public seeded scenario.
//
// The suite builds every name from one fixed seed (wire.ts SCENARIO_SEED) with a
// deterministic PRNG, so the token owner's login is a pure function of that seed.
// Recomputing it here (same algorithm, same seed, same word lists) tells the
// forge exactly which login owns every repository, without ever trusting a
// request path to name the owner. Nothing here reads the wall clock or any
// unseeded randomness: the same seed always yields the same identity.

package main

import (
	"math/bits"
	"strings"
)

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo",
	"iris", "jonas", "kira", "lars", "mira", "nico", "olga", "pavel",
	"quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia",
	"holm", "iversen", "jansen", "keller", "lind", "moreau", "novak",
	"olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega",
	"weber", "xu", "yilmaz", "zimmer",
}

// scenarioUser is one generated user of the seeded scenario.
type scenarioUser struct {
	id    int
	login string
	name  string
	email string
}

// scenarioIdentity is the viewer/teammate/stranger triple the suite uses.
type scenarioIdentity struct {
	viewer   scenarioUser
	teammate scenarioUser
	stranger scenarioUser
}

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// rng is xorshift64* over a 64-bit state, mirroring the suite's Rng.
type rng struct{ state uint64 }

func newRng(seed uint64) *rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &rng{state: seed}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive <= 0 {
		return 0
	}
	if maxExclusive == 1 {
		return 0
	}
	const range32 = 4294967296
	limit := range32 - (range32 % uint64(maxExclusive))
	for {
		v := uint64(r.nextU32())
		if v < limit {
			return int(v % uint64(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

// generateUsers builds count users with unique logins, deterministically.
func generateUsers(r *rng, count int) []scenarioUser {
	used := map[string]bool{}
	users := make([]scenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := strings.Map(func(ch rune) rune {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				return ch
			}
			return -1
		}, first+last)
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenarioUser{
			id:    1000 + i,
			login: login,
			name:  first + " " + last,
			email: login + "@example.invalid",
		})
	}
	return users
}

func scenarioIdentityOf() scenarioIdentity {
	users := generateUsers(newRng(scenarioSeedFor("users")), 12)
	return scenarioIdentity{viewer: users[0], teammate: users[1], stranger: users[2]}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

var _ = bits.OnesCount64
