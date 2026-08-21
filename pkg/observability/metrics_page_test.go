// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package observability

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The registry is fixed and the reference page enumerates it. Nothing
// kept the two in agreement until #228, and the drift is silent in the
// direction that matters: a family added without a page edit is a metric
// no operator's dashboard learns exists, and a family renamed leaves a
// page that reads like documentation and behaves like a broken query.
//
// So these tests compare the page against a real scrape rather than
// against the code that produces one. That distinction is the point.
// core-agent derived its expected names from the naming rules and got
// two of them wrong (a `_USD_total` suffix, a `By` unit that produced no
// `_bytes`); a gate that regenerates names from prometheus.CounterOpts
// inherits whatever the construction site gets wrong. New() + Prime()
// + a GET on Handler() is the published artifact — names, label names,
// and, because Prime materializes every enumerated value, the fixed
// vocabularies too.

const (
	// gateWorkload is the workload label Prime is called with. Any
	// value works; a distinctive one makes a mis-parse obvious.
	gateWorkload = "gate-fixture"

	metricsPagePath = "docs/site/src/content/docs/reference/metrics.md"
	designDocPath   = "docs/observability-design.md"

	// The design doc's shipped inventory is delimited so this test can
	// tell it apart from the much longer design-target catalog below it,
	// which names families that deliberately do not exist yet.
	inventoryStart = "shipped-metric-families:start"
	inventoryEnd   = "shipped-metric-families:end"
)

// ---- the code side: an actual scrape -----------------------------------

// scrapedFamily is one metric family as it appears on /metrics.
type scrapedFamily struct {
	kind     string                         // "counter" | "histogram" | ...
	labels   map[string]map[string]struct{} // label name -> values seen
	suffixes map[string]struct{}            // histogram series suffixes seen
}

var (
	typeLineRE  = regexp.MustCompile(`^# TYPE (\S+) (\S+)$`)
	labelPairRE = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="([^"]*)"`)
)

// scrapeRegistry primes a fresh registry and parses what Handler serves.
func scrapeRegistry(t *testing.T) map[string]*scrapedFamily {
	t.Helper()

	r := New()
	r.Prime(gateWorkload)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: status %d, want 200", rec.Code)
	}

	fams := map[string]*scrapedFamily{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if m := typeLineRE.FindStringSubmatch(line); m != nil {
			fams[m[1]] = &scrapedFamily{
				kind:     m[2],
				labels:   map[string]map[string]struct{}{},
				suffixes: map[string]struct{}{},
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		// <name>[{labels}] <value>
		series := line
		if brace := strings.Index(series, "{"); brace >= 0 {
			end := strings.LastIndex(series, "}")
			if end < brace {
				t.Fatalf("scrape: unbalanced label braces in %q", line)
			}
			series = series[:end+1]
		} else if sp := strings.Index(series, " "); sp >= 0 {
			series = series[:sp]
		}

		name, labels := splitSeries(series)
		fam, suffix := resolveFamily(fams, name)
		if fam == nil {
			t.Fatalf("scrape: series %q has no # TYPE line", name)
		}
		if suffix != "" {
			fam.suffixes[suffix] = struct{}{}
			delete(labels, "le") // a bucket boundary is not a vocabulary
		}
		for k, v := range labels {
			if fam.labels[k] == nil {
				fam.labels[k] = map[string]struct{}{}
			}
			fam.labels[k][v] = struct{}{}
		}
	}

	if len(fams) == 0 {
		t.Fatal("scrape parsed no families — the gate would pass by measuring nothing")
	}
	return fams
}

// splitSeries pulls the metric name and label pairs out of one series.
func splitSeries(series string) (string, map[string]string) {
	name := series
	labels := map[string]string{}
	if brace := strings.Index(series, "{"); brace >= 0 {
		name = series[:brace]
		for _, m := range labelPairRE.FindAllStringSubmatch(series[brace:], -1) {
			labels[m[1]] = m[2]
		}
	}
	return name, labels
}

// resolveFamily maps a series name to its family, unwrapping the
// _bucket/_sum/_count series a histogram exposes.
func resolveFamily(fams map[string]*scrapedFamily, name string) (*scrapedFamily, string) {
	if f, ok := fams[name]; ok {
		return f, ""
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		base := strings.TrimSuffix(name, suffix)
		if base == name {
			continue
		}
		if f, ok := fams[base]; ok && f.kind == "histogram" {
			return f, suffix
		}
	}
	return nil, ""
}

// ---- the page side -----------------------------------------------------

// pageFamily is one row of the reference page's family tables.
type pageFamily struct {
	line    int
	labels  map[string]struct{}
	vocab   map[string]struct{}
	meaning string
}

var (
	famCellRE  = regexp.MustCompile("^`(mast_[a-z0-9_]+)`$")
	backtickRE = regexp.MustCompile("`([^`]+)`")
	// A vocabulary member is a bare lowercase identifier. This is what
	// keeps prose out of the comparison: `--watchdog=enforce` and
	// `_bucket` are backticked in the same cells and are not values.
	vocabValueRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// vocabMarkers introduce an enumerated label vocabulary in a Meaning
// cell. Backticks before the marker are prose and are ignored.
var vocabMarkers = []string{"Outcomes:", "Operations:", "Sources:", "kind:"}

func parseMetricsPage(t *testing.T) map[string]*pageFamily {
	t.Helper()

	body := readRepoFile(t, metricsPagePath)
	out := map[string]*pageFamily{}
	for i, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := tableCells(line)
		if len(cells) < 3 {
			continue
		}
		m := famCellRE.FindStringSubmatch(cells[0])
		if m == nil {
			continue
		}
		name := m[1]
		if prev, dup := out[name]; dup {
			t.Errorf("%s:%d: %s is documented twice (also line %d) — two rows drift independently",
				metricsPagePath, i+1, name, prev.line)
			continue
		}
		pf := &pageFamily{
			line:    i + 1,
			labels:  map[string]struct{}{},
			vocab:   map[string]struct{}{},
			meaning: cells[2],
		}
		for _, b := range backtickRE.FindAllStringSubmatch(cells[1], -1) {
			pf.labels[b[1]] = struct{}{}
		}
		for _, b := range backtickRE.FindAllStringSubmatch(vocabSegment(cells[2]), -1) {
			if vocabValueRE.MatchString(b[1]) {
				pf.vocab[b[1]] = struct{}{}
			}
		}
		out[name] = pf
	}

	if len(out) == 0 {
		t.Fatalf("%s: parsed no family rows — the gate would pass by measuring nothing", metricsPagePath)
	}
	return out
}

// vocabSegment returns the part of a Meaning cell that enumerates a
// label vocabulary, or "" when the cell enumerates nothing.
func vocabSegment(meaning string) string {
	best := -1
	for _, marker := range vocabMarkers {
		if idx := strings.Index(meaning, marker); idx >= 0 && (best < 0 || idx < best) {
			best = idx + len(marker)
		}
	}
	if best < 0 {
		return ""
	}
	return meaning[best:]
}

func tableCells(line string) []string {
	parts := strings.Split(strings.TrimSpace(line), "|")
	if len(parts) < 2 {
		return nil
	}
	cells := make([]string, 0, len(parts))
	for _, p := range parts[1 : len(parts)-1] { // drop the leading/trailing empties
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- the gate ----------------------------------------------------------

// TestMetricsPageNamesEveryFamily is the two-way name match. A family
// the registry constructs and the page omits is undiscoverable; a
// `mast_*` the page names and the registry does not construct is a
// dashboard query that returns nothing.
func TestMetricsPageNamesEveryFamily(t *testing.T) {
	scraped := scrapeRegistry(t)
	page := parseMetricsPage(t)

	for _, name := range sortedKeys(scraped) {
		if _, ok := page[name]; !ok {
			t.Errorf("%s constructs %s and %s does not document it — add a row to the matching table",
				"pkg/observability", name, metricsPagePath)
		}
	}
	for _, name := range sortedKeys(page) {
		if _, ok := scraped[name]; !ok {
			t.Errorf("%s:%d documents %s and no family by that name is exported — it was renamed or removed",
				metricsPagePath, page[name].line, name)
		}
	}
}

// TestMetricsPagePinsLabelsAndVocabularies compares the label names and
// the enumerated label values. Prime materializes every value in every
// fixed vocabulary, so the scrape carries them and a value gained or
// renamed in pkg/observability shows up here rather than in a dashboard.
func TestMetricsPagePinsLabelsAndVocabularies(t *testing.T) {
	scraped := scrapeRegistry(t)
	page := parseMetricsPage(t)

	for _, name := range sortedKeys(scraped) {
		pf, ok := page[name]
		if !ok {
			continue // reported by TestMetricsPageNamesEveryFamily
		}
		fam := scraped[name]

		// Label names, both directions.
		for _, label := range sortedKeys(fam.labels) {
			if _, ok := pf.labels[label]; !ok {
				t.Errorf("%s:%d: %s is exported with label %q and the Labels column omits it",
					metricsPagePath, pf.line, name, label)
			}
		}
		for _, label := range sortedKeys(pf.labels) {
			if _, ok := fam.labels[label]; !ok {
				t.Errorf("%s:%d: the Labels column claims %s carries %q and the exported family does not",
					metricsPagePath, pf.line, name, label)
			}
		}

		// Every family with a vocabulary has exactly one enumerated
		// label beside `workload`. If that ever stops being true the
		// comparison below silently merges two vocabularies, so fail
		// loudly instead and extend the gate.
		var enumerated []string
		for _, label := range sortedKeys(fam.labels) {
			if label != "workload" {
				enumerated = append(enumerated, label)
			}
		}
		if len(enumerated) > 1 {
			t.Errorf("%s carries more than one enumerated label (%v); this gate compares one vocabulary per family and needs extending",
				name, enumerated)
			continue
		}
		if len(enumerated) == 0 {
			if len(pf.vocab) > 0 {
				t.Errorf("%s:%d: the page enumerates %v for %s, which has no label to carry them",
					metricsPagePath, pf.line, sortedKeys(pf.vocab), name)
			}
			continue
		}

		label := enumerated[0]
		for _, value := range sortedKeys(fam.labels[label]) {
			if _, ok := pf.vocab[value]; !ok {
				t.Errorf("%s:%d: %s{%s=%q} is exported and the page does not list it — the vocabulary gained a value quietly",
					metricsPagePath, pf.line, name, label, value)
			}
		}
		for _, value := range sortedKeys(pf.vocab) {
			if _, ok := fam.labels[label][value]; !ok {
				t.Errorf("%s:%d: the page lists %s{%s=%q} and nothing primes it — the value was renamed or dropped",
					metricsPagePath, pf.line, name, label, value)
			}
		}
	}
}

// TestMetricsPageDocumentsHistogramShape pins the one shape a counter
// table cannot describe: a histogram scrapes as three series, and an
// operator writing a query needs to know which one to reach for.
func TestMetricsPageDocumentsHistogramShape(t *testing.T) {
	scraped := scrapeRegistry(t)
	page := parseMetricsPage(t)

	histograms := 0
	for _, name := range sortedKeys(scraped) {
		fam := scraped[name]
		if fam.kind != "histogram" {
			continue
		}
		histograms++
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if _, ok := fam.suffixes[suffix]; !ok {
				t.Errorf("%s primes no %s%s series", name, name, suffix)
			}
			pf, ok := page[name]
			if !ok {
				continue // reported by TestMetricsPageNamesEveryFamily
			}
			if !strings.Contains(pf.meaning, "`"+suffix+"`") {
				t.Errorf("%s:%d: %s is a histogram and its row does not mention the `%s` series",
					metricsPagePath, pf.line, name, suffix)
			}
		}
	}
	if histograms == 0 {
		t.Fatal("no histogram families found — this test would pass by measuring nothing")
	}
}

// TestDesignDocInventoryNamesEveryFamily closes the drift #228 was found
// by: two shipped families appeared in neither column of the design
// doc's inventory, which is where a reader starting from the design
// corpus goes to learn what exists. Only one direction is checked — the
// delimited region may also name superseded sketches — and the reverse
// direction is the reference page's job, above.
func TestDesignDocInventoryNamesEveryFamily(t *testing.T) {
	scraped := scrapeRegistry(t)
	body := readRepoFile(t, designDocPath)

	start := strings.Index(body, inventoryStart)
	end := strings.Index(body, inventoryEnd)
	if start < 0 || end < 0 || end < start {
		t.Fatalf("%s: missing the %s / %s markers that delimit the shipped inventory",
			designDocPath, inventoryStart, inventoryEnd)
	}
	inventory := body[start:end]

	for _, name := range sortedKeys(scraped) {
		if !strings.Contains(inventory, name) {
			t.Errorf("%s: %s ships and the shipped-families inventory does not name it",
				designDocPath, name)
		}
	}
}
