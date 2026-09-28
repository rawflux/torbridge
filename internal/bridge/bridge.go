// Package bridge downloads, parses and tests Tor bridges for torbridge.
package bridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version of the torbridge release (torbridge and the installer packages).
const Version = "1.4.1"

type Source struct {
	Name string
	Base string // directory with <transport>_tested.txt files on raw.githubusercontent.com
}

// Sources are refreshed hourly and already tested by the collectors; the first one is the main one.
// Not every source has every transport (snowflake and conjure only in OnionHop).
var Sources = []Source{
	{"Delta-Kronecker", "https://raw.githubusercontent.com/Delta-Kronecker/Tor-Bridges-Collector/main/bridge/"},
	{"OnionHop", "https://raw.githubusercontent.com/center2055/OnionHop-Bridges-Collector/main/bridge/"},
	{"scriptzteam-v2", "https://raw.githubusercontent.com/scriptzteam/Tor-Bridges-Collector-v2/main/bridges/"},
}

type Bridge struct {
	Line      string // without the "Bridge" prefix
	Transport string // vanilla, obfs4, webtunnel, snowflake, meek_lite, conjure
	Addr      string // where we actually connect (host:port); placeholder for fronted transports
	SNI       string // webtunnel: TLS server name
	Source    string
	Builtin   bool // default bridge shipped with Tor Browser
}

// Probeable: fronted/brokered transports use placeholder addresses, a TCP probe says nothing.
func (b *Bridge) Probeable() bool {
	switch b.Transport {
	case "snowflake", "meek_lite", "conjure":
		return false
	}
	return true
}

// Host returns the IP or domain the client connects to.
func (b *Bridge) Host() string {
	h, _, _ := net.SplitHostPort(b.Addr)
	return h
}

// Port returns the port the client connects to.
func (b *Bridge) Port() int {
	_, p, _ := net.SplitHostPort(b.Addr)
	n, _ := strconv.Atoi(p)
	return n
}

// OperatorKey: IP for IP-addressed bridges, second-level domain for webtunnel.
func (b *Bridge) OperatorKey() string {
	host := b.Host()
	if b.SNI == "" {
		return host
	}
	if labels := strings.Split(host, "."); len(labels) > 2 {
		return strings.Join(labels[len(labels)-2:], ".")
	}
	return host
}

func Parse(s string) *Bridge {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "Bridge "))
	f := strings.Fields(s)
	if len(f) == 0 || strings.HasPrefix(f[0], "#") {
		return nil
	}
	b := &Bridge{Line: strings.Join(f, " "), Transport: "vanilla"}
	if !strings.Contains(f[0], ":") {
		b.Transport = f[0]
		f = f[1:]
	}
	if len(f) == 0 {
		return nil
	}
	b.Addr = f[0]
	if b.Transport == "webtunnel" {
		// A webtunnel address is a placeholder from 2001:db8::/32; the real endpoint is in url=.
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "url="); ok {
				u, err := url.Parse(v)
				if err != nil || u.Hostname() == "" {
					return nil
				}
				port := u.Port()
				if port == "" {
					port = "443"
				}
				b.SNI = u.Hostname()
				b.Addr = net.JoinHostPort(u.Hostname(), port)
			}
		}
		if b.SNI == "" {
			return nil
		}
	}
	if _, _, err := net.SplitHostPort(b.Addr); err != nil {
		return nil
	}
	return b
}

// ---------- download ----------

var reRaw = regexp.MustCompile(`^https://raw\.githubusercontent\.com/([^/]+)/([^/]+)/([^/]+)/(.+)$`)

// Fetch tries in order: GitHub directly, jsDelivr directly, GitHub through
// the tor SOCKS proxy (if socks != ""). Returns the data and the path that worked.
func Fetch(ctx context.Context, u, socks string) ([]byte, string, error) {
	urls := []string{u}
	if m := reRaw.FindStringSubmatch(u); m != nil {
		urls = append(urls, fmt.Sprintf("https://cdn.jsdelivr.net/gh/%s/%s@%s/%s", m[1], m[2], m[3], m[4]))
	}
	type way struct {
		name   string
		client *http.Client
		urls   []string
	}
	ways := []way{{"direct", &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}, urls}}
	if socks != "" {
		ways = append(ways, way{"via tor", &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "socks5", Host: socks}),
		}}, urls[:1]})
	}
	var lastErr error
	for _, w := range ways {
		for _, x := range w.urls {
			data, err := get(ctx, w.client, x)
			if err == nil {
				host, _ := url.Parse(x)
				return data, w.name + " " + host.Host, nil
			}
			lastErr = err
			if ctx.Err() != nil || errIs404(err) {
				break // 404: this source has no such list, mirrors will not have it either
			}
		}
		if errIs404(lastErr) {
			break
		}
	}
	return nil, "", lastErr
}

type httpStatus int

func (s httpStatus) Error() string { return fmt.Sprintf("HTTP %d", int(s)) }

func errIs404(err error) bool { s, ok := err.(httpStatus); return ok && s == 404 }

func get(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, httpStatus(resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}

// FetchAll downloads <type>_tested.txt of every type from every source and deduplicates.
func FetchAll(ctx context.Context, types []string, socks string, logf func(string, ...any)) []*Bridge {
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		byLine = map[string]*Bridge{}
	)
	for _, src := range Sources {
		for _, t := range types {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f := t + "_tested.txt"
				data, via, err := Fetch(ctx, src.Base+f, socks)
				if errIs404(err) {
					return
				}
				if err != nil {
					logf("  %s/%s: error: %s", src.Name, f, ShortErr(err))
					return
				}
				n := 0
				sc := bufio.NewScanner(strings.NewReader(string(data)))
				sc.Buffer(nil, 1<<20)
				for sc.Scan() {
					b := Parse(sc.Text())
					if b == nil || !want[b.Transport] {
						continue
					}
					n++
					mu.Lock()
					if _, ok := byLine[b.Line]; !ok {
						b.Source = src.Name
						byLine[b.Line] = b
					}
					mu.Unlock()
				}
				logf("  %s/%s: %d (%s)", src.Name, f, n, via)
			}()
		}
	}
	wg.Wait()
	out := make([]*Bridge, 0, len(byLine))
	for _, b := range byLine {
		out = append(out, b)
	}
	return out
}

// Builtin reads the default bridges shipped in pt_config.json of the Tor Expert Bundle.
func Builtin(ptConfig string, types []string) ([]*Bridge, error) {
	data, err := os.ReadFile(ptConfig)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Bridges map[string][]string `json:"bridges"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}
	var out []*Bridge
	for _, lines := range cfg.Bridges {
		for _, l := range lines {
			if b := Parse(l); b != nil && want[b.Transport] {
				b.Source, b.Builtin = "builtin", true
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// ---------- TCP/TLS probe ----------

// Probe connects n times (TLS with SNI for webtunnel); ok only if all attempts succeed.
func Probe(ctx context.Context, b *Bridge, n int, timeout time.Duration) (time.Duration, bool) {
	var worst time.Duration
	for i := range n {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		d := net.Dialer{Timeout: timeout}
		start := time.Now()
		var conn net.Conn
		var err error
		if b.SNI != "" {
			td := tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: b.SNI}}
			conn, err = td.DialContext(ctx, "tcp", b.Addr)
		} else {
			conn, err = d.DialContext(ctx, "tcp", b.Addr)
		}
		if err != nil {
			return 0, false
		}
		conn.Close()
		worst = max(worst, time.Since(start))
	}
	return worst, true
}

// ProbeAll probes bridges in parallel; returns probe results by bridge.
func ProbeAll(ctx context.Context, bridges []*Bridge, workers, n int, timeout time.Duration) map[*Bridge]time.Duration {
	jobs := make(chan *Bridge)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		alive = map[*Bridge]time.Duration{}
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				if rtt, ok := Probe(ctx, b, n, timeout); ok {
					mu.Lock()
					alive[b] = rtt
					mu.Unlock()
				}
			}
		}()
	}
	for _, b := range bridges {
		if ctx.Err() != nil {
			break
		}
		jobs <- b
	}
	close(jobs)
	wg.Wait()
	return alive
}

func Shuffle[T any](s []T) { rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] }) }

// ---------- real tor ----------

// Tor describes the local Tor Expert Bundle layout (same on Windows and Linux).
type Tor struct {
	Root string // C:\Tor or /opt/torbridge
}

// DefaultRoot is where the installers put Tor.
func DefaultRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\Tor`
	}
	return "/opt/torbridge"
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func (t Tor) Dir() string      { return filepath.Join(t.Root, "tor") }
func (t Tor) Exe() string      { return filepath.Join(t.Dir(), exe("tor")) }
func (t Tor) PTDir() string    { return filepath.Join(t.Dir(), "pluggable_transports") }
func (t Tor) PTConfig() string { return filepath.Join(t.PTDir(), "pt_config.json") }
func (t Tor) Defaults() string { return filepath.Join(t.Root, "torrc-defaults") }
func (t Tor) GeoIP() string    { return filepath.Join(t.Root, "data", "geoip") }
func (t Tor) GeoIP6() string   { return filepath.Join(t.Root, "data", "geoip6") }

// PluginLine returns the ClientTransportPlugin command for a transport ("" for vanilla).
func (t Tor) PluginLine(transport string) string {
	switch transport {
	case "vanilla":
		return ""
	case "conjure":
		return Slash(filepath.Join(t.PTDir(), exe("conjure-client"))) + " -registerURL https://registration.refraction.network/api"
	default: // obfs4, webtunnel, meek_lite, snowflake
		return Slash(filepath.Join(t.PTDir(), exe("lyrebird")))
	}
}

type TestOpts struct {
	Exit         string        // ExitNodes, "" for any
	Target       string        // URL that must open through the exit
	BootTimeout  time.Duration // whole bootstrap
	StallTimeout time.Duration // no bootstrap progress for this long = fail (0 = off)
}

type Result struct {
	OK        bool          // bootstrapped and target reachable
	Booted    bool          // bootstrap reached 100%
	Progress  int           // max bootstrap percent
	Tag       string        // last bootstrap phase tag, e.g. "handshake_done"
	Bootstrap time.Duration // time to 100%
	Err       string
	Warn      string // last tor warning, explains most failures
}

var reBoot = regexp.MustCompile(`Bootstrapped (\d+)% \(([a-z_]+)\)`)

const ErrAborted = "aborted"

// Test runs a separate tor with this single bridge, waits for bootstrap and
// requests the target twice on different circuits.
func (t Tor) Test(ctx context.Context, b *Bridge, o TestOpts) (r Result) {
	if ctx.Err() != nil {
		r.Err = ErrAborted
		return r
	}
	dir, err := os.MkdirTemp("", "torbridge-")
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer removeAllRetry(dir)
	port, err := FreePort()
	if err != nil {
		r.Err = err.Error()
		return r
	}

	var rc strings.Builder
	fmt.Fprintf(&rc, "DataDirectory %s\n", Slash(dir))
	fmt.Fprintf(&rc, "SocksPort 127.0.0.1:%d IsolateSOCKSAuth\n", port)
	fmt.Fprintf(&rc, "GeoIPFile %s\nGeoIPv6File %s\n", Slash(t.GeoIP()), Slash(t.GeoIP6()))
	rc.WriteString("Log notice stdout\nAvoidDiskWrites 1\nUseBridges 1\n")
	fmt.Fprintf(&rc, "Bridge %s\n", b.Line)
	if pl := t.PluginLine(b.Transport); pl != "" {
		fmt.Fprintf(&rc, "ClientTransportPlugin %s exec %s\n", b.Transport, pl)
	}
	if o.Exit != "" {
		fmt.Fprintf(&rc, "ExitNodes %s\nStrictNodes 1\n", o.Exit)
	}
	torrc := filepath.Join(dir, "torrc")
	if err := os.WriteFile(torrc, []byte(rc.String()), 0o600); err != nil {
		r.Err = err.Error()
		return r
	}

	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(tctx, t.Exe(), "-f", torrc, "--defaults-torrc", t.Defaults())
	Prepare(cmd, t.Dir())
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	start := time.Now()
	if err := cmd.Start(); err != nil {
		r.Err = err.Error()
		return r
	}
	// When tor exits it closes the transport's stdin, and the transport exits too.
	defer func() { cancel(); cmd.Wait() }()

	var (
		progress atomic.Int32
		lastMove atomic.Int64
		tag      atomic.Value
		warn     atomic.Value
	)
	lastMove.Store(time.Now().UnixNano())
	tag.Store("")
	warn.Store("")
	booted := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if l := sc.Text(); (strings.Contains(l, "[warn]") || strings.Contains(l, "[err]")) && !strings.Contains(l, "is relative and will resolve") {
				if i := strings.Index(l, "] "); i >= 0 {
					warn.Store(Trunc(l[i+2:], 160))
				}
			}
			if m := reBoot.FindStringSubmatch(sc.Text()); m != nil {
				p, _ := strconv.Atoi(m[1])
				if int32(p) > progress.Load() {
					progress.Store(int32(p))
					tag.Store(m[2])
					lastMove.Store(time.Now().UnixNano())
					if p == 100 {
						close(booted)
					}
				}
			}
		}
	}()
	// r is a named result, so these values survive every return below.
	defer func() {
		r.Progress, r.Tag = int(progress.Load()), tag.Load().(string)
		if !r.OK {
			r.Warn = warn.Load().(string)
		}
	}()

	deadline := time.After(o.BootTimeout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
wait:
	for {
		select {
		case <-booted:
			r.Booted, r.Bootstrap = true, time.Since(start)
			break wait
		case <-deadline:
			r.Err = fmt.Sprintf("bootstrap timeout %s", o.BootTimeout)
			return r
		case <-tick.C:
			if o.StallTimeout > 0 && time.Since(time.Unix(0, lastMove.Load())) > o.StallTimeout {
				r.Err = fmt.Sprintf("bootstrap stalled %s", o.StallTimeout)
				return r
			}
		case <-ctx.Done():
			r.Err = ErrAborted
			return r
		}
	}

	var lastErr error
	for i := range 2 {
		if err := HTTPVia(ctx, fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("t%d", i), o.Target); err == nil {
			r.OK = true
			return r
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			r.Err = ErrAborted
			return r
		}
	}
	r.Err = "target unreachable: " + ShortErr(lastErr)
	return r
}

// HTTPVia GETs target through SOCKS; any HTTP response means the target is reachable.
// A new SOCKS login means a new circuit (IsolateSOCKSAuth).
func HTTPVia(ctx context.Context, socks, user, target string) error {
	pu := &url.URL{Scheme: "socks5", Host: socks, User: url.UserPassword(user+strconv.Itoa(rand.IntN(1e9)), "x")}
	c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu), DisableKeepAlives: true}}
	req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ---------- helpers ----------

// removeAllRetry: the transport may hold files in the directory for a few seconds after tor exits.
func removeAllRetry(dir string) {
	for range 10 {
		if os.RemoveAll(dir) == nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// Slash: tor accepts forward slashes in Windows paths, while a backslash is an escape in torrc.
func Slash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func ShortErr(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return Trunc(err.Error(), 90)
}

func Trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "..."
	}
	return s
}
