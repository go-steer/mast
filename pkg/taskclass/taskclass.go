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

// Originally derived from go-steer/core-agent@cafe3106cf61cb7c1edbb39c2ce446dd87358747

// Package taskclass implements the --task flag's profile lookup —
// the operator-declared task-class story from
// docs/model-selection-design.md (issue #123).
//
// Six canonical classes (debug, implement, chat, research, review,
// orchestrate — the public set fixed by docs/orchestration-design.md
// "Public task classes") each map to a Profile that wraps a
// model-tier hint, compaction threshold, agentic-tools posture, and
// ask-mode default. The CLI applies the profile to whichever flags
// the operator left unspecified — explicit flags always win.
//
// The mast-side integration (class → ADK agent mode, per-class
// DefaultInstruction composition) lives in modes.go; this file stays
// close to the parent project's shape so future rate/table bumps
// diff cleanly.
//
// Tier classification (frontier / mid / small) shares vocabulary
// with pkg/modeltier but the resolution is per-provider here because
// we need to pick a SPECIFIC model ID (not just a class label).
// Hard-coded per-provider map for v1 per the design doc's Open
// Question 1 — pricing catalog has no tier field today, and
// inferring tier from price changes the wrong way (a price drop
// shouldn't reclassify a model).
//
// IAP / shape-of-future-work notes:
//
//   - Adding a sixth class (e.g. "monitor" for long-running
//     autonomous): add to Classes + canonical().
//   - Adding a provider (e.g. OpenAI): extend ModelForTier and
//     the per-provider switch in canonical().
//   - Tier-to-model when a new model ships: bump the per-provider
//     table here. The model-tier classifier in pkg/modeltier handles
//     the reverse (model → tier) and gets bumped separately.

package taskclass

// Canonical task-class names. Use these constants rather than string
// literals so future class renames are mechanically findable.
//
// Orchestrate is mast-added (the parent project ships five classes):
// bundles that enable the planner declare task_class: orchestrate,
// which maps to a planner-enabled Task-mode root (modes.go).
const (
	Debug       = "debug"
	Implement   = "implement"
	Chat        = "chat"
	Research    = "research"
	Review      = "review"
	Orchestrate = "orchestrate"
)

// Tier names mirror pkg/modeltier's TierFrontier / TierMid / TierSmall —
// duplicated as constants here so taskclass can be referenced without
// pulling in modeltier when only the labels are needed. Resolution
// (which provider's model for which tier) lives in ModelForTier below.
const (
	TierFrontier = "frontier"
	TierMid      = "mid"
	TierSmall    = "small"
)

// Ask-mode aliases for the AskMode field. The CLI's --ask flag
// accepts these strings + "yolo" + "plan" + "acceptEdits"; the ones
// listed here are the only values task-class profiles actually use.
const (
	AskAuto  = "auto"
	AskAsk   = "ask"
	AskAllow = "allow"
)

// Profile is the bundle a task class maps to. Applied to whatever
// flags the operator left unspecified; explicit flags win. All
// fields are optional in the sense that an empty / zero value means
// "don't override the substrate / operator default" — the CLI's
// resolution logic walks each field independently.
type Profile struct {
	// Tier hints which model class to pick. Resolved to a specific
	// model ID per-provider via ModelForTier. Empty = don't change
	// the model.
	Tier string

	// CompactionThreshold goes into the compactor's fallback
	// Threshold field. 0 = leave the substrate default in place.
	// Note: per-tier overrides from config still win for their
	// specific tier (see compactor's resolveThreshold precedence).
	CompactionThreshold float64

	// AgenticToolsEnabled is the desired agentic-tools state. The
	// substrate already defaults to on (PR #118), so today every
	// profile sets this true and the field is mostly informational.
	// Stays as an explicit field so a future "monitor" class that
	// wants agentic-tools off can express that.
	AgenticToolsEnabled bool

	// UseAgenticSmallModel controls whether agentic subtasks route
	// through a cheap-tier model (true) or inherit the parent's
	// model (false). True for tool-heavy task classes; false for
	// chat where subtask overhead doesn't pay off.
	UseAgenticSmallModel bool

	// AskMode is the desired permissions ask-mode default. Empty =
	// don't override the operator / config setting.
	AskMode string
}

// canonical is the source-of-truth profile table. Numbers track the
// design doc (docs/model-selection-design.md §"Piece 1"). Bumping a
// threshold here changes default behavior across every consumer that
// uses --task=<that class>; do it with intent.
func canonical() map[string]Profile {
	return map[string]Profile{
		Debug: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.65,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
		},
		Implement: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.70,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
		},
		Chat: {
			Tier:                 TierMid,
			CompactionThreshold:  0.85,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: false, // chat subtasks are usually one-shot reads; overhead doesn't pay off
			AskMode:              AskAuto,
		},
		Research: {
			Tier:                 TierMid,
			CompactionThreshold:  0.65,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAllow, // research is read-heavy; ask-mode noise is operator-hostile
		},
		Review: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.75,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
		},
		// Orchestrate is mast-added: the planner decomposes and
		// delegates, so it wants frontier reasoning, and its own
		// context stays plan-shaped (specialist output arrives as
		// digests), so the implement-class threshold carries over.
		Orchestrate: {
			Tier:                 TierFrontier,
			CompactionThreshold:  0.70,
			AgenticToolsEnabled:  true,
			UseAgenticSmallModel: true,
			AskMode:              AskAuto,
		},
	}
}

// Resolve returns the Profile for class. Empty class returns
// (Profile{}, false) — caller should not apply anything. Unknown
// class also returns (Profile{}, false); caller is expected to
// surface a useful error listing Classes().
func Resolve(class string) (Profile, bool) {
	if class == "" {
		return Profile{}, false
	}
	p, ok := canonical()[class]
	return p, ok
}

// Classes returns the canonical task-class names in a stable order
// suitable for CLI usage messages and validation errors. Order
// reflects the design doc's table layout (debug, implement, chat,
// research, review) rather than alphabetical so the most common
// operator choices appear first; orchestrate appends last as the
// mast-added planner class.
func Classes() []string {
	return []string{Debug, Implement, Chat, Research, Review, Orchestrate}
}

// Providers returns the provider names ModelForTier has tier
// mappings for. Extend together with ModelForTier's switch when a
// provider is added — consumers iterate this list to verify
// cross-table invariants for every default (pricing's
// TestBuiltin_CoversTaskclassTierDefaults walks Providers() x tiers,
// so a provider missing here silently loses that coverage).
func Providers() []string {
	return []string{"gemini", "vertex", "anthropic", "anthropic-vertex"}
}

// ModelForTier returns the default model ID for a (provider, tier)
// pair. Returns "" when no mapping exists — caller should fall
// through to whatever model would've been chosen without --task.
//
// Provider names match the --provider aliases internal/compose
// dispatches on ("gemini", "vertex", "anthropic", "anthropic-vertex")
// — the set Providers() returns. Offline fakes (echo, scripted,
// toolactor) don't appear here — they have no tier concept.
//
// The table embeds knowledge that also lives in pkg/modeltier's
// reverse direction (model → tier). When a new model ships, both
// need bumping. Not worth fusing into one table (the two directions
// have different shape needs: modeltier wants substring matching,
// taskclass wants canonical-string outputs).
//
// POLICY: each entry names the LATEST model in its line. Picking Opus
// means picking the newest Opus, not whichever Opus was current when
// the line was last edited. That is not enforceable from LiteLLM —
// the catalog has no recency field, and auto-promoting would ship an
// un-UAT'd model to every operator on a Monday regen — so it is
// enforced instead by TestModelForTier_ReturnsLatestInLine, which
// fails the build when pricing.Builtin() contains a newer model in the
// same line than the one returned here.
func ModelForTier(provider, tier string) string {
	switch provider {
	case "gemini", "vertex":
		switch tier {
		case TierFrontier:
			// gemini-3.7-flash: the current top of the flash-first
			// agentic line. Promoted 2026-08-17 off a live
			// Vertex UAT — all 31 judged corpus scenarios ran through
			// it, scoring within noise of the 3.6-era board, with no
			// mid-plan stall. That UAT is the bar: the parent project
			// shipped an un-UAT'd frontier bump on the strength of a
			// spec sheet (core-agent#579) and reverted it a day later
			// (#580) when the parent agent stopped mid-plan.
			//
			// This entry USED to justify itself partly on price — "half
			// the per-token cost of the gemini-3.6-flash it replaced,
			// $0.75/$3.75 against $1.50/$7.50". That was wrong by
			// 2026-08-19 and was never a durable argument. Google put
			// 3.6-flash onto the same $0.75/$3.75 introductory rate, and
			// BOTH revert to $1.50/$7.50 on 2027-01-01 (recorded in
			// pkg/pricing's introductoryRates, which fails the build if
			// the table has not moved by then). Promotion here rests on
			// the UAT; price parity between the two is temporary and
			// price is not what a frontier default is chosen on.
			//
			// The ported table originally said gemini-3.5-pro — a
			// model id that never shipped (inherited from core-agent,
			// stale there too; corrected 2026-07-29 when the first
			// live-credential run hit it).
			return "gemini-3.7-flash"
		case TierMid:
			// gemini-3.5-flash, not the older 2.5-pro: mid-tier
			// classes (research, chat) need built-in grounding to
			// coexist with function tools, which Gemini supports only
			// from 3.0 on — on 2.5-pro the research class literally
			// could not search (observed live 2026-07-29: the model
			// hallucinated a `search` tool and then apologized). Also
			// cheaper per the pricing catalog, and modeltier already
			// classifies the 3.5-flash line as mid.
			return "gemini-3.5-flash"
		case TierSmall:
			// gemini-3.5-flash-lite: current-gen budget tier at the
			// same price point as the 2.5-flash it replaced
			// ($0.30/$2.50 per MTok), with far stronger agentic
			// scores and a March 2026 knowledge cutoff. The 2.5-flash
			// it replaced also predates the Gemini 3.0 line's support
			// for built-ins alongside function declarations, so every
			// small-tier specialist silently ran unGrounded (see
			// builtinsCompatible in pkg/providers/gemini).
			return "gemini-3.5-flash-lite"
		}
	case "anthropic", "anthropic-vertex":
		switch tier {
		case TierFrontier:
			// Latest in the Opus line. Deliberately Opus and not the
			// Mythos-class tier (claude-fable-5 / claude-mythos-5),
			// which sits above Opus at 2x the rate — "frontier" is the
			// top of the general-purpose line, not the most expensive
			// model on offer.
			return "claude-opus-5"
		case TierMid:
			return "claude-sonnet-5"
		case TierSmall:
			// claude-haiku-4-5 is still the latest Haiku — no
			// 5-generation Haiku has shipped. Moves in lockstep with
			// pkg/providers/anthropic's DefaultSmallModelID; pinned by
			// TestModelForTier_ConsistentWithSmallModelDefaulters.
			return "claude-haiku-4-5"
		}
	}
	return ""
}
