package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"torbridge/internal/bridge"
)

// stats is a manual diagnostic: it tests an equal random sample of every bridge
// type (including types update does not use), records every result in the
// history, writes all working bridges to bridges-found.conf and, with -apply,
// applies the best of them to the system tor.
func stats(ctx context.Context) int {
	start := time.Now()
	exit, err := readExitNodes()
	if err != nil {
		logf("error: %v", err)
		return exitFailed
	}
	types := strings.Split(cfg.statsTypes, ",")
	run := "stats-" + start.Format("20060102-150405")
	logf("=== stats %s; types %s; exit %s; target %s", run, cfg.statsTypes, exit, cfg.target)

	bridges := bridge.FetchAll(ctx, types, cfg.socks, logf)
	if builtin, err := bridge.Builtin(tor().PTConfig(), types); err == nil {
		bridges = append(bridges, builtin...)
		logf("  builtin (pt_config.json): %d", len(builtin))
	}
	if len(bridges) == 0 {
		logf("error: no bridges downloaded - no network?")
		return exitNoNetwork
	}

	var probeable []*bridge.Bridge
	for _, b := range bridges {
		if b.Probeable() {
			probeable = append(probeable, b)
		}
	}
	logf("stage 1: TCP/TLS-probing %d bridges", len(probeable))
	alive := bridge.ProbeAll(ctx, probeable, cfg.workers, cfg.probes, cfg.probeTimeout)
	if ctx.Err() != nil {
		return exitFailed
	}

	geo := bridge.LoadGeoIP(tor().GeoIP(), tor().GeoIP6())
	recs := map[*bridge.Bridge]*bridge.Record{}
	byKind := map[string][]*bridge.Bridge{}
	for _, b := range bridges {
		r := bridge.NewRecord(run, b, geo)
		rtt, ok := alive[b]
		r.TCPOK, r.RTTms = ok, int(rtt.Milliseconds())
		recs[b] = &r
		if !b.Probeable() || ok {
			byKind[bridge.Kind(b)] = append(byKind[bridge.Kind(b)], b)
		}
	}

	// Equal random sample per kind, one bridge per operator, interleaved so that
	// every kind is tested under the same network conditions.
	var kinds []string
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	picked := map[string][]*bridge.Bridge{}
	for _, k := range kinds {
		bs := byKind[k]
		bridge.Shuffle(bs)
		seen := map[string]bool{}
		for _, b := range bs {
			if len(picked[k]) >= cfg.sample {
				break
			}
			if key := b.OperatorKey(); b.Probeable() && seen[key] {
				continue
			} else {
				seen[key] = true
			}
			picked[k] = append(picked[k], b)
		}
		logf("  %-18s usable %4d, sampled %d", k, len(bs), len(picked[k]))
	}
	var queue []*bridge.Bridge
	for i := 0; ; i++ {
		added := false
		for _, k := range kinds {
			if i < len(picked[k]) {
				queue = append(queue, picked[k][i])
				added = true
			}
		}
		if !added {
			break
		}
	}

	logf("stage 2: testing %d bridges with a real tor (%d in parallel)", len(queue), cfg.parallel)
	opts := bridge.TestOpts{Exit: exit, Target: cfg.target, BootTimeout: cfg.bootTimeout, StallTimeout: cfg.stallTimeout}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		next  int
		done  int
		found []*bridge.Bridge
		boot  = map[*bridge.Bridge]time.Duration{}
	)
	for range cfg.parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(queue) || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				b := queue[next]
				next++
				mu.Unlock()

				r := tor().Test(ctx, b, opts)
				if r.Err == bridge.ErrAborted {
					return
				}
				mu.Lock()
				done++
				recs[b].SetResult(r)
				status := fmt.Sprintf("OK %s", r.Bootstrap.Round(time.Second))
				if r.OK {
					found = append(found, b)
					boot[b] = r.Bootstrap
				} else {
					status = fmt.Sprintf("FAIL at %d%% (%s): %s", r.Progress, r.Tag, r.Err)
				}
				logf("  [%3d/%d] %-18s %-40s %s", done, len(queue), bridge.Kind(b), bridge.Trunc(b.Addr, 40), status)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	out := make([]bridge.Record, 0, len(recs))
	for _, b := range bridges {
		out = append(out, *recs[b])
	}
	if err := bridge.AppendHistory(cfg.history, out); err != nil {
		logf("history: %v", err)
	}
	fmt.Printf("\n=== this run (%d records appended to %s)\n", len(out), cfg.history)
	printReport(out)

	// The fastest working bridges first.
	sort.Slice(found, func(i, j int) bool { return boot[found[i]] < boot[found[j]] })
	if len(found) == 0 {
		logf("no working bridges found")
		return exitFailed
	}
	foundConf := path("bridges-found.conf")
	if err := writeAtomic(foundConf, []byte(renderBridges(found, exit))); err != nil {
		logf("error: %v", err)
		return exitFailed
	}
	logf("%d working bridges written to %s (ready-made bridges.conf)", len(found), foundConf)
	if !cfg.applyFound {
		return exitOK
	}
	if len(found) < cfg.minGood {
		logf("error: fewer than %d working bridges - system tor left unchanged", cfg.minGood)
		return exitFailed
	}
	best := found[:min(len(found), cfg.want)]
	if err := apply(ctx, best, exit); err != nil {
		logf("error: %v", err)
		return exitFailed
	}
	saveState(State{LastSuccess: time.Now(), Bridges: len(best)})
	logf("=== applied %d bridges, connection works (total %s)", len(best), time.Since(start).Round(time.Second))
	return exitOK
}

func report() int {
	recs, err := bridge.LoadHistory(cfg.history, cfg.since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitFailed
	}
	runs := map[string]bool{}
	for _, r := range recs {
		runs[r.Run] = true
	}
	fmt.Printf("=== history: %d records, %d runs (%s)\n", len(recs), len(runs), cfg.history)
	printReport(recs)
	return exitOK
}

// ---------- report tables ----------

type agg struct {
	listed, probed, tcpOK       int
	tested, booted, ok          int
	early, mid, late, targetErr int // where failed tests stopped
	boots                       []float64
}

func (a *agg) add(r bridge.Record) {
	a.listed++
	if r.Probed {
		a.probed++
		if r.TCPOK {
			a.tcpOK++
		}
	}
	if !r.Tested {
		return
	}
	a.tested++
	switch {
	case r.OK:
		a.ok++
		a.booted++
		a.boots = append(a.boots, r.BootS)
	case r.Booted:
		a.booted++
		a.targetErr++
	case r.Progress < 10:
		a.early++
	case r.Progress < 75:
		a.mid++
	default:
		a.late++
	}
}

func pct(n, d int) string {
	if d == 0 {
		return "-"
	}
	return strconv.Itoa(100*n/d) + "%"
}

func (a *agg) okRate() float64 {
	if a.tested == 0 {
		return -1
	}
	return float64(a.ok) / float64(a.tested)
}

func medianF(v []float64) string {
	if len(v) == 0 {
		return "-"
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return fmt.Sprintf("%.0fs", s[len(s)/2])
}

func group(recs []bridge.Record, key func(bridge.Record) string) (map[string]*agg, []string) {
	m := map[string]*agg{}
	for _, r := range recs {
		k := key(r)
		if k == "" {
			continue
		}
		if m[k] == nil {
			m[k] = &agg{}
		}
		m[k].add(r)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if ri, rj := m[keys[i]].okRate(), m[keys[j]].okRate(); ri != rj {
			return ri > rj
		}
		return keys[i] < keys[j]
	})
	return m, keys
}

func printReport(recs []bridge.Record) {
	byKind, kinds := group(recs, func(r bridge.Record) string { return r.Kind })
	fmt.Println("\nBy kind (sorted by success of real-tor tests):")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "kind\trecords\tTCP ok\ttested\tbootstrap\tworks\tmedian boot\tstuck <10%\t10-74%\t75-99%\texit/target\t")
	for _, k := range kinds {
		a := byKind[k]
		tcp := "n/a"
		if a.probed > 0 {
			tcp = pct(a.tcpOK, a.probed)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t\n", k, a.listed, tcp, a.tested,
			pct(a.booted, a.tested), pct(a.ok, a.tested), medianF(a.boots), a.early, a.mid, a.late, a.targetErr)
	}
	w.Flush()
	fmt.Println("  TCP ok: bridge address reachable. stuck <10%: connection closed during the")
	fmt.Println("  transport handshake. works: bootstrap reached 100% and the target opened.")

	fmt.Println("\nBy group (as used for update priorities, tested only):")
	byGroup, groups := group(recs, func(r bridge.Record) string {
		if !r.Tested || r.Kind != r.Transport {
			return ""
		}
		return bridge.Group(r.Transport, r.Port)
	})
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "group\ttested\tworks\tstuck <10%\t")
	for _, k := range groups {
		a := byGroup[k]
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t\n", k, a.tested, pct(a.ok, a.tested), a.early)
	}
	w.Flush()

	fmt.Println("\nBy bridge country (tested only, 3+ tests):")
	byCC, ccs := group(recs, func(r bridge.Record) string {
		if !r.Tested || r.Country == "" {
			return ""
		}
		return r.Country
	})
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "country\ttested\tworks\t")
	for _, k := range ccs {
		if a := byCC[k]; a.tested >= 3 {
			fmt.Fprintf(w, "%s\t%d\t%s\t\n", k, a.tested, pct(a.ok, a.tested))
		}
	}
	w.Flush()
}
