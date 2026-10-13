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

// Command pricing-diff prints core-models' built-in price table, one
// sorted line per row, for dev/ci/pricing-diff.sh to diff between two
// pins.
//
// It lives in mast's module so build and lint keep it compiling against
// the current pin; the script copies it into a scratch module per
// version, because one build cannot import two versions of the same
// module.
//
// UpdatedAt is left out on purpose: every regeneration restamps it, and
// a report that listed every row as changed would hide the rows whose
// price did.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/go-steer/core-models/pricing"
)

func main() {
	rows := map[string]pricing.Rates{}
	for k, r := range pricing.Builtin() {
		rows[k] = r
	}
	for k, r := range pricing.BuiltinByBackend() {
		rows[k] = r
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s\t%s\n", k, format(rows[k]))
	}
	if _, err := os.Stdout.WriteString(b.String()); err != nil {
		fmt.Fprintln(os.Stderr, "pricing-diff:", err)
		os.Exit(1)
	}
}

// format renders a row's rates in USD per million tokens, with zero
// buckets omitted so a row reads as what it charges.
func format(r pricing.Rates) string {
	parts := buckets(r.InputPerMTok, r.CachedInputPerMTok, r.CacheCreationInputPerMTok, r.CacheCreation1hInputPerMTok, r.OutputPerMTok)
	if lc := r.LongContext; lc != nil {
		parts = append(parts, fmt.Sprintf("above %dK: %s", lc.ThresholdTokens/1000,
			strings.Join(buckets(lc.InputPerMTok, lc.CachedInputPerMTok, lc.CacheCreationInputPerMTok, lc.CacheCreation1hInputPerMTok, lc.OutputPerMTok), " ")))
	}
	return strings.Join(parts, " ")
}

func buckets(in, cached, write, write1h, out float64) []string {
	var parts []string
	for _, f := range []struct {
		name string
		v    float64
	}{{"in", in}, {"cached", cached}, {"write", write}, {"write1h", write1h}, {"out", out}} {
		if f.v != 0 {
			parts = append(parts, fmt.Sprintf("%s=%g", f.name, f.v))
		}
	}
	return parts
}
