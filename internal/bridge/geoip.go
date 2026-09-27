package bridge

import (
	"bufio"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
)

// GeoIP maps IP addresses to country codes using tor's geoip/geoip6 files.
type GeoIP struct {
	ranges []geoRange // sorted by lo
}

type geoRange struct {
	lo, hi netip.Addr
	cc     string
}

// LoadGeoIP reads tor's geoip (IPv4 as integers) and geoip6 (IPv6 text) files.
// Missing files give an empty database.
func LoadGeoIP(v4, v6 string) *GeoIP {
	g := &GeoIP{}
	g.load(v4, func(s string) (netip.Addr, bool) {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
	})
	g.load(v6, func(s string) (netip.Addr, bool) {
		a, err := netip.ParseAddr(s)
		return a, err == nil
	})
	sort.Slice(g.ranges, func(i, j int) bool { return g.ranges[i].lo.Less(g.ranges[j].lo) })
	return g
}

func (g *GeoIP) load(name string, parse func(string) (netip.Addr, bool)) {
	f, err := os.Open(name)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' {
			continue
		}
		p := strings.Split(l, ",")
		if len(p) != 3 {
			continue
		}
		lo, ok1 := parse(p[0])
		hi, ok2 := parse(p[1])
		if ok1 && ok2 {
			g.ranges = append(g.ranges, geoRange{lo, hi, strings.ToLower(p[2])})
		}
	}
}

// Country returns the lowercase country code of an IP, "" for domains or unknown addresses.
func (g *GeoIP) Country(host string) string {
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return ""
	}
	a = a.Unmap()
	i := sort.Search(len(g.ranges), func(i int) bool { return a.Less(g.ranges[i].lo) }) - 1
	if i >= 0 && a.BitLen() == g.ranges[i].lo.BitLen() && !g.ranges[i].hi.Less(a) {
		return g.ranges[i].cc
	}
	return ""
}
