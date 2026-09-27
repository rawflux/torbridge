package bridge

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
)

// Group splits bridges into classes with different observed success rates;
// for IP-addressed transports the port (443/80 or other) is part of the class.
func Group(transport string, port int) string {
	switch transport {
	case "vanilla", "obfs4":
		if port == 443 || port == 80 {
			return transport + "@443/80"
		}
		return transport + "@other"
	}
	return transport
}

// DefaultRates are initial success-rate estimates from earlier measurements;
// the local test history refines them.
var DefaultRates = map[string]float64{
	"webtunnel":      0.65,
	"vanilla@443/80": 0.40,
	"vanilla@other":  0.45,
	"obfs4@443/80":   0.50,
	"obfs4@other":    0.20,
	"snowflake":      0.05,
	"meek_lite":      0.05,
	"conjure":        0.05,
}

const (
	priorWeight = 5    // the prior counts as this many tests
	unknownRate = 0.3  // prior for groups without a default
	minWeight   = 0.02 // even a weak group is tried eventually
)

// Stats holds success counts from the history.
type Stats struct {
	groups map[string][2]int // group -> ok, tested
	types  map[string][2]int // transport -> ok, tested
	Tests  int
}

// NewStats counts real-tor tests of list bridges (builtin bridges are not in the lists).
func NewStats(recs []Record) *Stats {
	s := &Stats{groups: map[string][2]int{}, types: map[string][2]int{}}
	for _, r := range recs {
		if !r.Tested || r.Kind != r.Transport {
			continue
		}
		s.Tests++
		add := func(m map[string][2]int, k string) {
			c := m[k]
			c[1]++
			if r.OK {
				c[0]++
			}
			m[k] = c
		}
		add(s.groups, Group(r.Transport, r.Port))
		add(s.types, r.Transport)
	}
	return s
}

// Rate is the smoothed success rate of a group.
func (s *Stats) Rate(group string) float64 {
	prior, ok := DefaultRates[group]
	if !ok {
		prior = unknownRate
	}
	c := s.groups[group]
	return (float64(c[0]) + priorWeight*prior) / (float64(c[1]) + priorWeight)
}

// TypeRate returns the raw success rate of a transport and the number of tests.
func (s *Stats) TypeRate(transport string) (float64, int) {
	c := s.types[transport]
	if c[1] == 0 {
		return 0, 0
	}
	return float64(c[0]) / float64(c[1]), c[1]
}

// Order sorts candidates by weighted random sampling without replacement
// (Efraimidis-Spirakis): a bridge from a group with twice the success rate is
// about twice as likely to be tried earlier, yet every group keeps a chance.
// Returns the ordered list and a description of the group weights.
func (s *Stats) Order(cands []*Bridge) ([]*Bridge, string) {
	type keyed struct {
		b   *Bridge
		key float64
	}
	ks := make([]keyed, len(cands))
	count := map[string]int{}
	for i, b := range cands {
		g := Group(b.Transport, b.Port())
		count[g]++
		w := max(s.Rate(g), minWeight)
		ks[i] = keyed{b, math.Pow(rand.Float64(), 1/w)}
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].key > ks[j].key })
	out := make([]*Bridge, len(ks))
	for i, k := range ks {
		out[i] = k.b
	}

	groups := make([]string, 0, len(count))
	for g := range count {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return s.Rate(groups[i]) > s.Rate(groups[j]) })
	parts := make([]string, len(groups))
	for i, g := range groups {
		parts[i] = fmt.Sprintf("%s %.0f%% (%d)", g, 100*s.Rate(g), count[g])
	}
	return out, strings.Join(parts, ", ")
}
