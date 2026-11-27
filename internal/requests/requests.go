// Package requests reads a workload's own request counter straight from each
// of its pods, in the Prometheus text format, the way Prometheus itself would
// scrape it. No Prometheus server is involved. A Tracker turns the readings
// into a short history: how many requests the workload got between two reads,
// and how many of them failed with 5xx.
package requests

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// MinReadInterval is how often, at most, one guard's pods are read.
	MinReadInterval = 4 * time.Second
	// MaxPods is how many of a workload's pods are read, at most.
	MaxPods = 20

	podTimeout   = 2 * time.Second // per pod
	readTimeout  = 3 * time.Second // for all the pods of one read
	parallel     = 8               // pods read at the same time
	maxBody      = 8 << 20         // bytes per answer
	maxLine      = 1 << 20         // bytes per line of an answer
	maxSamples   = 2000            // per guard
	maxErrorText = 200             // characters, so an answer is never echoed into status

	accept = "text/plain;version=0.0.4;q=0.9,*/*;q=0.1"
)

// Count is a number of requests and how many of them failed with 5xx.
type Count struct {
	Total, Failed float64
}

// Over reports whether more than limit percent of the requests failed.
// Exactly at the limit is not over, and no requests are never over.
func (c Count) Over(limit float64) bool {
	return c.Total > 0 && c.Failed*100 > limit*c.Total
}

// Percent is the share of the requests that failed, in percent.
func (c Count) Percent() float64 {
	if c.Total <= 0 {
		return 0
	}
	return c.Failed * 100 / c.Total
}

// Sample is what all of a workload's pods counted since the previous read,
// stamped with the time of the read.
type Sample struct {
	At time.Time
	Count
}

// Series is a workload's samples, oldest first.
type Series []Sample

// Between sums the samples taken after from and up to to.
func (s Series) Between(from, to time.Time) Count {
	var c Count
	for _, x := range s {
		if x.At.After(from) && !x.At.After(to) {
			c.Total += x.Total
			c.Failed += x.Failed
		}
	}
	return c
}

// Config says where a workload counts its requests.
type Config struct {
	Port        int32
	Path        string
	Metric      string
	StatusLabel string
}

// URL is where a pod serves its metrics. An IPv6 address is bracketed.
func (c Config) URL(t Target) string {
	return "http://" + net.JoinHostPort(t.IP, strconv.Itoa(int(c.Port))) + c.Path
}

// Target is a pod to read.
type Target struct {
	Name string
	UID  types.UID
	IP   string
}

// Targets picks the pods to read: running, with an IP, not being deleted,
// and not on the host's network, whose ports are the node's. At most
// MaxPods, by name.
func Targets(pods []corev1.Pod) []Target {
	var out []Target
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" || p.Spec.HostNetwork {
			continue
		}
		out = append(out, Target{Name: p.Name, UID: p.UID, IP: p.Status.PodIP})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > MaxPods {
		out = out[:MaxPods]
	}
	return out
}

// Parse reads the metric from an answer in the Prometheus text format and
// sums its series. Every series counts toward the total; one whose
// statusLabel starts with 5 counts as failed too. Only the metric's own lines
// are parsed, so a malformed line of another metric can't spoil the read.
func Parse(r io.Reader, metric, statusLabel string) (Count, error) {
	text, err := keep(r, metric)
	if err != nil {
		return Count{}, err
	}
	p := expfmt.NewTextParser(model.UTF8Validation)
	families, err := p.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		return Count{}, fmt.Errorf("reading %s: %w", metric, err)
	}
	family, ok := families[metric]
	if !ok {
		family, ok = families[strings.TrimSuffix(metric, "_count")]
	}
	if !ok {
		return Count{}, fmt.Errorf("no %s metric", metric)
	}
	var c Count
	labelled := false
	for _, m := range family.GetMetric() {
		var v float64
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			v = m.GetCounter().GetValue()
		case dto.MetricType_UNTYPED:
			v = m.GetUntyped().GetValue()
		case dto.MetricType_HISTOGRAM:
			v = float64(m.GetHistogram().GetSampleCount())
		case dto.MetricType_SUMMARY:
			v = float64(m.GetSummary().GetSampleCount())
		default:
			return Count{}, fmt.Errorf("%s is a %s, not a counter", metric, strings.ToLower(family.GetType().String()))
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return Count{}, fmt.Errorf("%s has the value %g, not a count", metric, v)
		}
		c.Total += v
		for _, l := range m.GetLabel() {
			if l.GetName() == statusLabel {
				labelled = true
				if strings.HasPrefix(l.GetValue(), "5") {
					c.Failed += v
				}
			}
		}
	}
	if !labelled {
		// Better said than read as no failures at all.
		return Count{}, fmt.Errorf("no %s series has a %s label", metric, statusLabel)
	}
	return c, nil
}

// keep returns the lines of the answer that are about the metric: its HELP
// and TYPE lines, and its samples. For a histogram's or summary's base name,
// that includes its _count samples.
func keep(r io.Reader, metric string) (string, error) {
	base := strings.TrimSuffix(metric, "_count")
	counts := false // the metric is a histogram's or summary's base name
	var b strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " \t")
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(line)
			if len(f) < 3 || f[0] != "#" || (f[1] != "TYPE" && f[1] != "HELP") || (f[2] != metric && f[2] != base) {
				continue
			}
			if f[1] == "TYPE" && f[2] == metric && len(f) > 3 && (f[3] == "histogram" || f[3] == "summary") {
				counts = true
			}
		} else if !sampleOf(line, metric) && !(counts && sampleOf(line, metric+"_count")) {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// sampleOf reports whether a sample line is one of name's.
func sampleOf(line, name string) bool {
	if !strings.HasPrefix(line, name) {
		return false
	}
	if len(line) == len(name) {
		return true // a sample with no value: the parser refuses it
	}
	switch line[len(name)] {
	case '{', ' ', '\t':
		return true
	}
	return false
}

// errTooBig: Flagbreaker refuses an answer rather than count part of it.
var errTooBig = fmt.Errorf("the answer is over %d MiB", maxBody>>20)

// limitReader fails once more than left bytes were read.
type limitReader struct {
	r    io.Reader
	left int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, errTooBig
	}
	return n, err
}

// Result is what one Read found.
type Result struct {
	// Series is the guard's history after the read.
	Series Series
	// Skipped is true when the pods were read less than MinReadInterval
	// ago, so they weren't read again.
	Skipped bool
	// Read is how many of the Of pods answered.
	Read, Of int
	// Err is the first pod's error, when a pod didn't answer.
	Err error
}

// Tracker reads guards' workloads and keeps each guard's history. The history
// lives in memory only: a controller restart starts it over.
type Tracker struct {
	// HTTP reads the pods.
	HTTP *http.Client

	mu     sync.Mutex
	guards map[types.NamespacedName]*history
}

// NewTracker reads pods with client. With nil, it uses a client that gives
// each pod 2 seconds, follows no redirects and never goes through a proxy.
func NewTracker(client *http.Client) *Tracker {
	if client == nil {
		client = &http.Client{
			Timeout: podTimeout,
			Transport: &http.Transport{
				Proxy:               nil, // never HTTP_PROXY: these are pod IPs
				DialContext:         (&net.Dialer{Timeout: podTimeout}).DialContext,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     30 * time.Second,
			},
			// A redirect is an answer like any other non-200: an error.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Tracker{HTTP: client}
}

type podKey struct {
	// Both: the fake client in tests leaves UIDs empty, and StatefulSet pods
	// reuse names.
	name string
	uid  types.UID
}

type history struct {
	mu       sync.Mutex
	uid      types.UID
	cfg      Config
	workload string
	lastRead time.Time
	pods     map[podKey]Count // each pod's last reading
	series   Series
}

// Read reads the workload's pods and adds what they counted since the last
// read, summed, to the guard's history, as one sample stamped now. It reads
// at most once every MinReadInterval per guard, and keeps `keep` of history.
//
// A pod's first reading is only where its counting starts. A reading below
// the last one means the container restarted, so the pod counts from zero. A
// pod that doesn't answer keeps its last reading, and when no pod answers,
// nothing is added. The history starts over when the guard (by UID), the
// config or the workload changes.
func (t *Tracker) Read(ctx context.Context, guard types.NamespacedName, uid types.UID, cfg Config, workload string,
	targets []Target, now time.Time, keep time.Duration) Result {
	h := t.history(guard)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pods == nil || h.uid != uid || h.cfg != cfg || h.workload != workload {
		h.uid, h.cfg, h.workload = uid, cfg, workload
		h.lastRead, h.pods, h.series = time.Time{}, map[podKey]Count{}, nil
	}
	if since := now.Sub(h.lastRead); !h.lastRead.IsZero() && since >= 0 && since < MinReadInterval {
		return Result{Series: append(Series(nil), h.series...), Skipped: true}
	}
	h.lastRead = now

	readings := t.readAll(ctx, cfg, targets)
	res := Result{Of: len(targets)}
	var delta Count
	current := make(map[podKey]bool, len(targets))
	for i, target := range targets {
		key := podKey{name: target.Name, uid: target.UID}
		current[key] = true
		r := readings[i]
		if r.err != nil {
			if res.Err == nil {
				res.Err = r.err
			}
			continue
		}
		res.Read++
		last, seen := h.pods[key]
		h.pods[key] = r.count
		switch {
		case !seen:
			// Only where this pod's counting starts.
		case r.count.Total < last.Total || r.count.Failed < last.Failed:
			// The container restarted, and its counter with it.
			delta.Total += r.count.Total
			delta.Failed += r.count.Failed
		default:
			delta.Total += r.count.Total - last.Total
			delta.Failed += r.count.Failed - last.Failed
		}
	}
	for key := range h.pods {
		if !current[key] {
			delete(h.pods, key) // no longer one of the workload's running pods
		}
	}
	if res.Read > 0 {
		h.series = append(h.series, Sample{At: now, Count: delta})
	}
	h.trim(now.Add(-keep))
	res.Series = append(Series(nil), h.series...)
	return res
}

// Forget drops a guard's history.
func (t *Tracker) Forget(guard types.NamespacedName) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.guards, guard)
}

func (t *Tracker) history(guard types.NamespacedName) *history {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.guards == nil {
		t.guards = map[types.NamespacedName]*history{}
	}
	h, ok := t.guards[guard]
	if !ok {
		h = &history{}
		t.guards[guard] = h
	}
	return h
}

// trim drops the samples taken before oldest, and keeps at most maxSamples.
func (h *history) trim(oldest time.Time) {
	i := 0
	for i < len(h.series) && h.series[i].At.Before(oldest) {
		i++
	}
	if len(h.series)-i > maxSamples {
		i = len(h.series) - maxSamples
	}
	if i > 0 {
		h.series = append(Series(nil), h.series[i:]...)
	}
}

type reading struct {
	count Count
	err   error
}

// readAll reads the targets, at most `parallel` at a time, all within
// readTimeout.
func (t *Tracker) readAll(ctx context.Context, cfg Config, targets []Target) []reading {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	out := make([]reading, len(targets))
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				out[i].err = podError(targets[i].Name, "not read within %s", readTimeout)
				return
			}
			out[i].count, out[i].err = t.fetch(ctx, cfg, targets[i])
		}(i)
	}
	wg.Wait()
	return out
}

// fetch reads one pod.
func (t *Tracker) fetch(ctx context.Context, cfg Config, target Target) (Count, error) {
	url := cfg.URL(target)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Count{}, podError(target.Name, "%v", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "flagbreaker")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return Count{}, podError(target.Name, "%v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // a read-only body
	switch {
	case resp.StatusCode != http.StatusOK:
		return Count{}, podError(target.Name, "GET %s returned %d", url, resp.StatusCode)
	case strings.Contains(resp.Header.Get("Content-Type"), "protobuf"):
		return Count{}, podError(target.Name, "GET %s answered in protobuf; Flagbreaker reads the text format only", url)
	}
	c, err := Parse(&limitReader{r: resp.Body, left: maxBody}, cfg.Metric, cfg.StatusLabel)
	if err != nil {
		return Count{}, podError(target.Name, "GET %s: %v", url, err)
	}
	return c, nil
}

// podError names the pod, and caps the text at maxErrorText characters.
func podError(pod, format string, args ...any) error {
	msg := "pod " + pod + ": " + fmt.Sprintf(format, args...)
	if r := []rune(msg); len(r) > maxErrorText {
		msg = string(r[:maxErrorText-3]) + "..."
	}
	return errors.New(msg)
}
