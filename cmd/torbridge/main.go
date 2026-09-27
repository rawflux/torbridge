// torbridge finds working Tor bridges and keeps the system tor on Windows running on them.
//
// Update cycle:
//  1. Download tested bridge lists from GitHub (three sources; if GitHub is
//     unreachable, the jsDelivr mirror, then GitHub through tor itself).
//  2. Shuffle and TCP/TLS-probe every bridge in parallel.
//  3. Test with a real tor (one process per bridge, exits only in allowed
//     countries) in parallel until -want bridges work.
//  4. Write bridges.conf, restart the tor service and verify the connection
//     via ControlPort and SocksPort. If it does not come up, restore the previous bridges.
//
// Commands: update (forced), auto (scheduled: update if the bridges are older
// than -max-age or the connection is down), check, status.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"torbridge/internal/bridge"
)

// Exit codes: the scheduler needs to tell "no network" from "no bridges found".
const (
	exitOK        = 0
	exitFailed    = 1
	exitNoNetwork = 2
)

var cfg struct {
	root         string
	types        string
	want         int
	minGood      int
	maxTested    int
	probes       int
	probeTimeout time.Duration
	workers      int
	parallel     int
	bootTimeout  time.Duration
	target       string
	maxAge       time.Duration
	socks        string
	control      string
	history      string
	statsWindow  time.Duration
	stallTimeout time.Duration
	typesSet     bool // -types given explicitly: no automatic transport selection
	// stats / report
	statsTypes string
	sample     int
	applyFound bool
	since      time.Duration
}

// Paths inside -root (C:\Tor by default); install.ps1 creates the layout.
func path(parts ...string) string { return filepath.Join(append([]string{cfg.root}, parts...)...) }

var (
	baseTorrc   = func() string { return path("torrc") }
	bridgesConf = func() string { return path("bridges.conf") }
	stateFile   = func() string { return path("torbridge-state.json") }
	cookieFile  = func() string { return path("state", "control_auth_cookie") }
	logFile     = func() string { return path("logs", "torbridge.log") }
)

type State struct {
	LastSuccess time.Time `json:"last_success"`
	Bridges     int       `json:"bridges"`
}

func main() {
	cmd := "status"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("torbridge", flag.ExitOnError)
	fs.StringVar(&cfg.root, "root", bridge.DefaultRoot(), "tor installation directory")
	// obfs4, snowflake, meek_lite and conjure showed low success rates in measurements;
	// update adds a transport by itself once its history success rate is high enough.
	fs.StringVar(&cfg.types, "types", "webtunnel,vanilla", "comma-separated transports (explicit value disables automatic selection)")
	fs.IntVar(&cfg.want, "want", 10, "how many working bridges to collect")
	fs.IntVar(&cfg.minGood, "min", 3, "below this the config is left unchanged")
	fs.IntVar(&cfg.maxTested, "max-tested", 80, "max bridges tested with a real tor per run")
	fs.IntVar(&cfg.probes, "probes", 2, "TCP/TLS probes per bridge (all must pass)")
	fs.DurationVar(&cfg.probeTimeout, "probe-timeout", 5*time.Second, "timeout of one TCP/TLS probe")
	fs.IntVar(&cfg.workers, "workers", 100, "parallel TCP/TLS probes")
	fs.IntVar(&cfg.parallel, "parallel", 4, "test tor processes running at once")
	fs.DurationVar(&cfg.bootTimeout, "bootstrap-timeout", 90*time.Second, "bootstrap timeout per bridge")
	fs.DurationVar(&cfg.stallTimeout, "stall-timeout", 60*time.Second, "fail a bridge whose bootstrap does not move for this long")
	fs.StringVar(&cfg.target, "target", "https://api.anthropic.com/", "URL that must be reachable through the exits")
	fs.DurationVar(&cfg.maxAge, "max-age", 7*24*time.Hour, "auto: refresh bridges older than this")
	fs.StringVar(&cfg.socks, "socks", "", "SocksPort of the system tor (default: from <root>/torrc)")
	fs.StringVar(&cfg.control, "control", "", "ControlPort of the system tor (default: from <root>/torrc)")
	fs.StringVar(&cfg.history, "history", "", "test history (default: <root>/logs/history.jsonl)")
	fs.DurationVar(&cfg.statsWindow, "stats-window", 30*24*time.Hour, "use history records newer than this for priorities")
	fs.StringVar(&cfg.statsTypes, "stats-types", strings.Join(allTypes, ","), "stats: transports to measure")
	fs.IntVar(&cfg.sample, "sample", 15, "stats: bridges of each kind tested with a real tor")
	fs.BoolVar(&cfg.applyFound, "apply", false, "stats: apply the best working bridges found to the system tor")
	fs.DurationVar(&cfg.since, "since", 0, "report: only records newer than this (e.g. 720h), 0 = all")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "torbridge %s\n\nUsage: torbridge <command> [flags]\n\n"+
			"  update   find bridges and apply them now\n"+
			"  auto     update if bridges are older than -max-age or the connection is down\n"+
			"  check    check the connection through the system tor\n"+
			"  status   show state (default)\n"+
			"  stats    diagnostics: test a sample of every bridge type, write the working\n"+
			"           ones to bridges-found.conf (-apply: also apply them)\n"+
			"  report   success statistics from the test history\n\nFlags:\n", bridge.Version)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "types" {
			cfg.typesSet = true
		}
	})
	if cfg.history == "" {
		cfg.history = path("logs", "history.jsonl")
	}
	// The installer may pick non-default ports, so the base torrc is the source of truth.
	if cfg.socks == "" {
		cfg.socks = torrcAddr("SocksPort", "127.0.0.1:9050")
	}
	if cfg.control == "" {
		cfg.control = torrcAddr("ControlPort", "127.0.0.1:9051")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd {
	case "update":
		os.Exit(locked(func() int { return update(ctx) }))
	case "auto":
		os.Exit(locked(func() int { return auto(ctx) }))
	case "check":
		if err := verify(ctx, 60*time.Second); err != nil {
			fmt.Println("connection is DOWN:", err)
			os.Exit(exitFailed)
		}
		fmt.Println("connection works")
	case "status":
		status(ctx)
	case "stats":
		os.Exit(locked(func() int { return stats(ctx) }))
	case "report":
		os.Exit(report())
	default:
		fs.Usage()
		os.Exit(exitFailed)
	}
}

// locked prevents two updates at once (scheduler + manual run).
// The port is released automatically even if the process crashes.
func locked(fn func() int) int {
	l, err := net.Listen("tcp", "127.0.0.1:9058")
	if err != nil {
		fmt.Fprintln(os.Stderr, "an update is already running")
		return exitFailed
	}
	defer l.Close()
	openLog()
	return fn()
}

func auto(ctx context.Context) int {
	st := loadState()
	age := time.Since(st.LastSuccess)
	if st.LastSuccess.IsZero() || age > cfg.maxAge {
		logf("auto: bridges last updated %s ago - updating", orNever(st.LastSuccess))
		return update(ctx)
	}
	if err := verify(ctx, 2*time.Minute); err != nil {
		logf("auto: connection is down (%v) - updating bridges", err)
		return update(ctx)
	}
	logf("auto: connection works, bridges updated %s ago - nothing to do", age.Round(time.Hour))
	return exitOK
}

func update(ctx context.Context) int {
	start := time.Now()
	logf("=== bridge update (torbridge %s)", bridge.Version)
	exit, err := readExitNodes()
	if err != nil {
		logf("error: %v", err)
		return exitFailed
	}
	logf("exit nodes: %s; target: %s", exit, cfg.target)

	hist, err := bridge.LoadHistory(cfg.history, cfg.statsWindow)
	if err != nil {
		logf("history: %v", err)
	}
	st := bridge.NewStats(hist)
	bridges := bridge.FetchAll(ctx, selectTypes(st), cfg.socks, logf)
	if len(bridges) == 0 {
		logf("error: no source returned bridges - no network?")
		return exitNoNetwork
	}
	logf("unique bridges downloaded: %d", len(bridges))

	probed := bridge.ProbeAll(ctx, bridges, cfg.workers, cfg.probes, cfg.probeTimeout)
	alive := make([]*bridge.Bridge, 0, len(probed))
	for b := range probed {
		alive = append(alive, b)
	}
	logf("stage 1: passed all %d TCP/TLS probes: %d", cfg.probes, len(alive))
	if ctx.Err() != nil {
		return exitFailed
	}
	if len(alive) == 0 {
		logf("error: no bridge answers - no network?")
		return exitNoNetwork
	}

	cands, desc := st.Order(diversify(alive))
	logf("group weights from %d tested bridges: %s", st.Tests, desc)
	logf("stage 2: testing with a real tor (up to %d working, %d in parallel, %d candidates)",
		cfg.want, cfg.parallel, len(cands))
	good, recs := findGood(ctx, cands, exit)
	if err := bridge.AppendHistory(cfg.history, recs); err != nil {
		logf("history: %v", err)
	}
	logf("working bridges found: %d (in %s)", len(good), time.Since(start).Round(time.Second))
	if ctx.Err() != nil {
		return exitFailed
	}
	if len(good) < cfg.minGood {
		logf("error: fewer than %d working bridges - config left unchanged", cfg.minGood)
		return exitFailed
	}

	if err := apply(ctx, good, exit); err != nil {
		logf("error: %v", err)
		return exitFailed
	}
	saveState(State{LastSuccess: time.Now(), Bridges: len(good)})
	logf("=== done: %d bridges, connection works (total %s)", len(good), time.Since(start).Round(time.Second))
	return exitOK
}

// torrcValue returns the value of the first "key value" line of the base torrc.
func torrcValue(key string) (string, error) {
	data, err := os.ReadFile(baseTorrc())
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), key+" "); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("no %s line in %s", key, baseTorrc())
}

// readExitNodes takes ExitNodes from the base torrc: one source of truth
// for both the system tor and the test instances.
func readExitNodes() (string, error) { return torrcValue("ExitNodes") }

// torrcAddr reads a port line such as "SocksPort 127.0.0.1:9150 IsolateSOCKSAuth".
func torrcAddr(key, def string) string {
	v, err := torrcValue(key)
	if err != nil || v == "" {
		return def
	}
	addr := strings.Fields(v)[0]
	if !strings.Contains(addr, ":") {
		addr = "127.0.0.1:" + addr
	}
	return addr
}

// ---------- stages ----------

// diversify shuffles the live bridges and keeps at most one per operator:
// bridges on one server tend to fail together.
func diversify(alive []*bridge.Bridge) []*bridge.Bridge {
	bridge.Shuffle(alive)
	seen := map[string]bool{}
	var out []*bridge.Bridge
	for _, b := range alive {
		if k := b.OperatorKey(); !seen[k] {
			seen[k] = true
			out = append(out, b)
		}
	}
	return out
}

// findGood tests candidates in order with cfg.parallel workers and stops
// as soon as cfg.want bridges work.
func findGood(ctx context.Context, cands []*bridge.Bridge, exit string) ([]*bridge.Bridge, []bridge.Record) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		next   int
		tested int
		good   []*bridge.Bridge
		recs   []bridge.Record
	)
	opts := bridge.TestOpts{Exit: exit, Target: cfg.target, BootTimeout: cfg.bootTimeout, StallTimeout: cfg.stallTimeout}
	geo := bridge.LoadGeoIP(tor().GeoIP(), tor().GeoIP6())
	run := "update-" + time.Now().Format("20060102-150405")
	limit := min(len(cands), cfg.maxTested)
	for range cfg.parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if len(good) >= cfg.want || next >= limit || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				b := cands[next]
				next++
				mu.Unlock()

				r := tor().Test(ctx, b, opts)

				mu.Lock()
				if ctx.Err() == nil || r.OK {
					tested++
				}
				if r.Err != bridge.ErrAborted {
					rec := bridge.NewRecord(run, b, geo)
					rec.SetResult(r)
					recs = append(recs, rec)
				}
				switch {
				case r.OK && len(good) < cfg.want:
					good = append(good, b)
					logf("  [%2d] OK   %-9s %-42s bootstrap %s", len(good), b.Transport, bridge.Trunc(b.Addr, 42), r.Bootstrap.Round(time.Second))
					if len(good) >= cfg.want {
						cancel() // enough: abort the remaining tests
					}
				case !r.OK && r.Err != bridge.ErrAborted:
					logf("       FAIL %-9s %-42s %s (%d%% %s)", b.Transport, bridge.Trunc(b.Addr, 42), r.Err, r.Progress, r.Tag)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	logf("tested with a real tor: %d", tested)
	return good, recs
}

// allTypes: every transport torbridge can use.
var allTypes = []string{"webtunnel", "vanilla", "obfs4", "snowflake", "meek_lite", "conjure"}

// Automatic transport selection: a transport outside -types is used once the
// history shows it works (at least autoRate of autoTests tests).
const (
	autoRate  = 0.4
	autoTests = 15
)

func selectTypes(st *bridge.Stats) []string {
	types := strings.Split(cfg.types, ",")
	if cfg.typesSet {
		return types
	}
	for _, t := range allTypes {
		if contains(types, t) {
			continue
		}
		if r, n := st.TypeRate(t); n >= autoTests && r >= autoRate {
			logf("transport %s enabled automatically: %.0f%% of %d tests worked", t, 100*r, n)
			types = append(types, t)
		}
	}
	return types
}

func tor() bridge.Tor { return bridge.Tor{Root: cfg.root} }

// ---------- applying to the system tor ----------

func renderBridges(good []*bridge.Bridge, exit string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# torbridge %s, %s; tested via exits %s, target %s\n",
		bridge.Version, time.Now().Format("2006-01-02 15:04"), exit, cfg.target)
	sb.WriteString("UseBridges 1\n")
	var pts []string
	for _, b := range good {
		if tor().PluginLine(b.Transport) != "" && !contains(pts, b.Transport) {
			pts = append(pts, b.Transport)
			fmt.Fprintf(&sb, "ClientTransportPlugin %s exec %s\n", b.Transport, tor().PluginLine(b.Transport))
		}
	}
	for _, b := range good {
		fmt.Fprintf(&sb, "Bridge %s\n", b.Line)
	}
	return sb.String()
}

// apply replaces bridges.conf, reloads tor and verifies the connection.
// If it does not come up, the previous file is restored.
func apply(ctx context.Context, good []*bridge.Bridge, exit string) error {
	old, readErr := os.ReadFile(bridgesConf())
	hadOld := readErr == nil && strings.Contains(string(old), "\nBridge ")
	if hadOld {
		if err := os.WriteFile(bridgesConf()+".bak", old, 0o644); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	if err := writeAtomic(bridgesConf(), []byte(renderBridges(good, exit))); err != nil {
		return err
	}
	logf("wrote %s", bridgesConf())

	// A restart, not SIGNAL RELOAD: tor does not switch to changed bridges
	// reliably on reload (tested: reload never came up, restart takes ~10 s).
	if err := restartTor(ctx); err != nil {
		logf("tor restart: %v", err)
	}
	err := verify(ctx, 4*time.Minute)
	if err == nil {
		return nil
	}
	if !hadOld {
		return fmt.Errorf("connection over the new bridges did not come up: %w (no previous bridges, keeping the new ones)", err)
	}
	logf("connection over the new bridges did not come up (%v) - restoring the previous ones", err)
	if werr := writeAtomic(bridgesConf(), old); werr != nil {
		return fmt.Errorf("rollback failed: %w", werr)
	}
	if rerr := restartTor(ctx); rerr != nil {
		logf("tor restart after rollback: %v", rerr)
	}
	if verr := verify(ctx, 4*time.Minute); verr != nil {
		logf("previous bridges do not work either: %v", verr)
	}
	return fmt.Errorf("new bridges did not work, previous ones restored: %w", err)
}

// verify waits for the system tor to reach bootstrap 100% and checks the target through SocksPort.
func verify(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p, err := bootstrapProgress()
		switch {
		case errors.Is(err, os.ErrPermission):
			return err // not a connection problem, waiting will not help
		case err != nil:
			lastErr = fmt.Errorf("ControlPort: %s", bridge.ShortErr(err))
		case p < 100:
			lastErr = fmt.Errorf("bootstrap %d%%", p)
		default:
			if err := bridge.HTTPVia(ctx, cfg.socks, "verify", cfg.target); err != nil {
				lastErr = fmt.Errorf("target unreachable: %s", bridge.ShortErr(err))
			} else {
				return nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	return lastErr
}

var reProgress = regexp.MustCompile(`PROGRESS=(\d+)`)

func bootstrapProgress() (int, error) {
	c, err := dialControl()
	if err != nil {
		return 0, err
	}
	defer c.Close()
	r, err := c.cmd("GETINFO status/bootstrap-phase")
	if err != nil {
		return 0, err
	}
	m := reProgress.FindStringSubmatch(r)
	if m == nil {
		return 0, fmt.Errorf("unexpected reply: %s", bridge.Trunc(r, 80))
	}
	return strconv.Atoi(m[1])
}

// ---------- ControlPort ----------

type control struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialControl() (*control, error) {
	conn, err := net.DialTimeout("tcp", cfg.control, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &control{conn: conn, r: bufio.NewReader(conn)}
	cookie, err := os.ReadFile(cookieFile())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("cookie: %w", err)
	}
	if _, err := c.cmd("AUTHENTICATE " + hex.EncodeToString(cookie)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("AUTHENTICATE: %w", err)
	}
	return c, nil
}

// cmd sends a command and reads the reply up to the final "NNN ..." line.
func (c *control) cmd(s string) (string, error) {
	c.conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprintf(c.conn, "%s\r\n", s); err != nil {
		return "", err
	}
	var sb strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return sb.String(), err
		}
		sb.WriteString(line)
		if len(line) >= 4 && line[3] == ' ' {
			if !strings.HasPrefix(line, "250") {
				return sb.String(), errors.New(strings.TrimSpace(line))
			}
			return sb.String(), nil
		}
	}
}

func (c *control) Close() { c.conn.Close() }

// ---------- status ----------

func status(ctx context.Context) {
	fmt.Printf("torbridge %s, directory %s\n\n", bridge.Version, cfg.root)
	fmt.Println("tor service:  ", serviceState())
	fmt.Println("ports:        ", "SOCKS", cfg.socks, "control", cfg.control)
	if p, err := bootstrapProgress(); err == nil {
		fmt.Printf("bootstrap:     %d%%\n", p)
	} else {
		fmt.Println("bootstrap:     unknown:", bridge.ShortErr(err))
	}
	st := loadState()
	if st.LastSuccess.IsZero() {
		fmt.Println("last update:   never")
	} else {
		fmt.Printf("last update:   %s (%s ago), %d bridges\n", st.LastSuccess.Format("2006-01-02 15:04"),
			time.Since(st.LastSuccess).Round(time.Minute), st.Bridges)
	}
	if exit, err := readExitNodes(); err == nil {
		fmt.Println("exit nodes:   ", exit)
	}
	if data, err := os.ReadFile(bridgesConf()); err == nil {
		n := map[string]int{}
		for _, l := range strings.Split(string(data), "\n") {
			if b := bridge.Parse(l); b != nil && strings.HasPrefix(l, "Bridge ") {
				n[b.Transport]++
			}
		}
		fmt.Println("bridges:      ", n)
	}
	fmt.Print("connection:    ")
	if err := verify(ctx, 20*time.Second); errors.Is(err, os.ErrPermission) {
		fmt.Println("unknown: no access to the ControlPort cookie, run as administrator/root")
	} else if err != nil {
		fmt.Println("DOWN:", err)
	} else {
		fmt.Println("works (target", cfg.target+")")
	}
}

// ---------- helpers ----------

func loadState() State {
	var st State
	if data, err := os.ReadFile(stateFile()); err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

func saveState(st State) {
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := writeAtomic(stateFile(), data); err != nil {
		logf("cannot write %s: %v", stateFile(), err)
	}
}

func writeAtomic(name string, data []byte) error {
	tmp := name + ".new"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name) // replaces the existing file on Windows
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func orNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Hour).String()
}

var logOut io.Writer = os.Stderr

// openLog duplicates the log to a file: there is no console when run by the scheduler.
func openLog() {
	os.MkdirAll(filepath.Dir(logFile()), 0o755)
	if f, err := os.OpenFile(logFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		logOut = io.MultiWriter(os.Stderr, f)
	}
}

func logf(format string, a ...any) {
	fmt.Fprintf(logOut, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}
