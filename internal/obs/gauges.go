package obs

import (
	"sort"
	"strings"
	"sync"
)

// Gauge is one current-value sample rendered on /metrics. Labels are
// alternating key, value pairs.
type Gauge struct {
	Name   string
	Help   string
	Labels []string
	Value  float64
}

var (
	gaugeMu  sync.Mutex
	gaugeSrc func() []Gauge
)

// SetGaugeSource registers the function that reports current state (running
// pipelines, open approvals, today's spend) at scrape time. It is called on
// every scrape, outside the metric registry lock; nil removes it.
func SetGaugeSource(f func() []Gauge) {
	gaugeMu.Lock()
	defer gaugeMu.Unlock()
	gaugeSrc = f
}

func renderGauges(sb *strings.Builder) {
	gaugeMu.Lock()
	src := gaugeSrc
	gaugeMu.Unlock()
	if src == nil {
		return
	}
	samples := src()
	byName := map[string][]Gauge{}
	help := map[string]string{}
	var names []string
	for _, g := range samples {
		if _, ok := byName[g.Name]; !ok {
			names = append(names, g.Name)
		}
		byName[g.Name] = append(byName[g.Name], g)
		if g.Help != "" {
			help[g.Name] = g.Help
		}
	}
	sort.Strings(names)
	for _, name := range names {
		sb.WriteString("# HELP " + name + " " + help[name] + "\n")
		sb.WriteString("# TYPE " + name + " gauge\n")
		for _, g := range byName[name] {
			sb.WriteString(name)
			if l := lbls(g.Labels...); l != "" {
				sb.WriteString("{" + l + "}")
			}
			sb.WriteString(" " + formatNum(g.Value) + "\n")
		}
	}
}
