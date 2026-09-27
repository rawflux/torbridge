package bridge

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Record is one line of the test history (JSONL). Every real-tor test made by
// "update" and "stats" is recorded, so statistics come from normal operation.
type Record struct {
	Run       string    `json:"run"` // "update-..." or "stats-..."
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"` // transport, "-builtin" for Tor Browser default bridges
	Transport string    `json:"transport"`
	Source    string    `json:"source"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Country   string    `json:"country,omitempty"`
	Probed    bool      `json:"probed"` // TCP/TLS probe applies to this kind
	TCPOK     bool      `json:"tcp_ok"`
	RTTms     int       `json:"rtt_ms,omitempty"`
	Tested    bool      `json:"tested"` // tested with a real tor
	Booted    bool      `json:"booted,omitempty"`
	OK        bool      `json:"ok,omitempty"` // booted and target reachable
	Progress  int       `json:"progress,omitempty"`
	Tag       string    `json:"tag,omitempty"`
	BootS     float64   `json:"boot_s,omitempty"`
	Err       string    `json:"err,omitempty"`
	Warn      string    `json:"warn,omitempty"`
}

// Kind separates the Tor Browser default bridges from list bridges of the same transport.
func Kind(b *Bridge) string {
	if b.Builtin {
		return b.Transport + "-builtin"
	}
	return b.Transport
}

// NewRecord describes a bridge; test results are filled in with SetResult.
func NewRecord(run string, b *Bridge, geo *GeoIP) Record {
	return Record{
		Run: run, Time: time.Now(), Kind: Kind(b), Transport: b.Transport, Source: b.Source,
		Host: b.Host(), Port: b.Port(), Country: geo.Country(b.Host()), Probed: b.Probeable(),
	}
}

func (r *Record) SetResult(res Result) {
	r.Tested, r.Booted, r.OK = true, res.Booted, res.OK
	r.Progress, r.Tag, r.Err, r.Warn = res.Progress, res.Tag, res.Err, res.Warn
	r.BootS = res.Bootstrap.Round(100 * time.Millisecond).Seconds()
}

func AppendHistory(name string, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	os.MkdirAll(filepath.Dir(name), 0o755)
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return w.Flush()
}

// LoadHistory reads records newer than window (0 = all). A missing file is an empty history.
func LoadHistory(name string, window time.Duration) ([]Record, error) {
	f, err := os.Open(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && (window == 0 || time.Since(r.Time) < window) {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
