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

package mast

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A comment in this module may *record* an API name that no longer
// resolves. It may not *instruct* a reader to call one.
//
// The distinction is the whole check. pkg/attach/prompter.go names
// `agent.WithAttachPromptBroker` deliberately, because a reader who
// remembers it from v0.8 needs to be told where it went — and it names
// `attachadapter.WithPromptBroker` beside it, because that is the one
// that compiles. The same file named the dead spelling as the thing to
// call until 2026-09-16, and it never existed in this repo: a consumer
// following the comment got a compile error and no hint (#364).
//
// (This file is inside the tree it walks, which is why the paragraph
// above carries both names. That is not a workaround — it is the rule,
// applied to the one comment most likely to name the dead spelling.)
//
// This is a guard rather than a one-time fix because the source of the
// bad comment is still upstream: core-agent renamed the option when it
// split pkg/agent, four of its own comments still name the old one, and
// pkg/attach is ported code. The next sync can carry the instruction
// straight back in. The port is the event this test is waiting for —
// see docs/sibling-sync.md, `532f7c75`, which is mast's own finding
// taken upstream; the guard is the half that did not come back.
//
// The rule it enforces is mechanical: a comment group naming a dead
// spelling must also name the live one. That does not prove the group
// is a record rather than an instruction, and it is not meant to — it
// guarantees the weaker thing that actually matters, which is that a
// reader who reaches the dead name in a comment reaches the working
// name without leaving it.

// renamedOption is one API spelling that no longer resolves, paired
// with the one that does.
//
// Add a row when a rename lands that consumers or ported comments will
// keep naming the old way. Do not add one for a name that was only ever
// internal: the cost of a row is that every mention of the dead name,
// including in prose about the rename itself, has to carry the live one
// alongside it.
type renamedOption struct {
	dead string
	live string
	// note is appended to the failure message. It is where the reason
	// this particular rename keeps coming back goes, because whoever
	// trips the check is usually mid-port and has no idea why the
	// comment they just copied is a problem.
	note string
}

var renamedOptions = []renamedOption{{
	dead: "agent.WithAttachPromptBroker",
	live: "attachadapter.WithPromptBroker",
	note: "This one arrives by port, not by typo: core-agent renamed the\n" +
		"\toption when it split pkg/agent and four of its own comments still\n" +
		"\tname the old spelling, so any sync that touches pkg/attach can\n" +
		"\tbring it back. It never existed in this repo under either name.\n" +
		"\tSee docs/sibling-sync.md (`532f7c75`) and #364.",
}}

// renamedOptionProblems returns one message per comment group in src
// that names a dead spelling without naming its replacement, together
// with a count, keyed by dead spelling, of the groups that named the
// live one.
//
// That second return is the check's floor. Every assertion here is a
// substring match against a name, so the whole thing goes quietly
// vacuous the day a name changes: nothing matches, nothing fails, and
// the walk reports a clean tree because it looked for something that is
// no longer there. A live spelling that appears nowhere in the module
// means this row is pinned to a name the code stopped using.
//
// It takes source text rather than a path so the checker itself can be
// exercised on fixtures — a tree walk that finds nothing is not
// evidence that it would find something.
func renamedOptionProblems(label, src string) ([]string, map[string]int) {
	sightings := make(map[string]int, len(renamedOptions))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, label, src, parser.ParseComments)
	if err != nil {
		return []string{label + ": parse: " + err.Error()}, sightings
	}
	var problems []string
	for _, group := range f.Comments {
		// Collapse the group before matching, twice over.
		//
		// A record long enough to explain itself wraps, and the half of
		// it that names the live spelling lands on a different line from
		// the half that names the dead one — so a line-by-line match
		// reports a correct record as a violation. prompter.go's record
		// is exactly that shape: the two names sit in different
		// paragraphs of one comment group.
		//
		// `squeezed` additionally drops the whitespace *inside* the
		// collapsed text, so a name wrapped mid-identifier ("agent.\n//
		// WithAttachPromptBroker") still matches. gofmt does not rewrap
		// comments, so that shape only appears when a human puts it
		// there — which is precisely the case where the automated check
		// is the only thing looking.
		flat := strings.Join(strings.Fields(group.Text()), " ")
		squeezed := strings.Map(func(r rune) rune {
			if r == ' ' {
				return -1
			}
			return r
		}, flat)
		for _, opt := range renamedOptions {
			if strings.Contains(squeezed, opt.live) {
				sightings[opt.dead]++
			}
			if !strings.Contains(squeezed, opt.dead) {
				continue
			}
			if strings.Contains(squeezed, opt.live) {
				continue
			}
			pos := fset.Position(group.Pos())
			problems = append(problems, label+":"+strconv.Itoa(pos.Line)+
				": this comment names "+opt.dead+", which does not\n"+
				"\tresolve, and does not name "+opt.live+"\n"+
				"\tanywhere in the same comment. A reader following it gets a\n"+
				"\tcompile error and no hint.\n"+
				"\n"+
				"\tTwo ways out. Delete the mention if nobody is owed it. Or keep\n"+
				"\tit as a record — say the old name is the old name and put the\n"+
				"\tworking spelling beside it — which is what\n"+
				"\tpkg/attach/prompter.go does.\n"+
				"\n"+
				"\t"+opt.note+"\n"+
				"\n"+
				"\tcomment: "+flat)
		}
	}
	return problems, sightings
}

func TestNoCommentInstructsCallingARenamedOption(t *testing.T) {
	var problems []string
	files := 0
	sightings := make(map[string]int, len(renamedOptions))
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata holds fixtures that are not this module's code;
			// node_modules is the docs site's; dist is build output.
			case ".git", "node_modules", "testdata", "dist":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		found, seen := renamedOptionProblems(path, string(src))
		problems = append(problems, found...)
		for dead, n := range seen {
			sightings[dead] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}

	// A walk that stopped finding files is not a green run. There were
	// 659 Go files when this was written (re-derive it with: find . -name
	// '*.go' -not -path './.git/*' -not -path '*/node_modules/*' -not
	// -path '*/testdata/*' -not -path './dist/*' | wc -l); the floor is
	// low enough that deleting a package does not trip it and high enough
	// that a broken walk does.
	if files < 300 {
		t.Errorf("examined %d Go files, expected at least 300; "+
			"this test is no longer checking what it thinks it is", files)
	}
	// Neither is a row whose live spelling nothing names. See the
	// comment on renamedOptionProblems: the rename moved again, or the
	// record this row protects was deleted, and either way the row is
	// now matching against a name that is not in the tree.
	for _, opt := range renamedOptions {
		if sightings[opt.dead] == 0 {
			t.Errorf("no comment in this module names %s, so the %s row "+
				"is pinned to a spelling the code no longer uses. Re-check the "+
				"rename and update or delete the row.", opt.live, opt.dead)
		}
	}
}

// The tree holds exactly one mention of a dead spelling and it is a
// correct record, so the walk above passes whether or not the checker
// works. This is what shows it would not.
func TestRenamedOptionCheckerReadsTheWholeComment(t *testing.T) {
	// The shape that shipped through v0.8: an instruction to call a name
	// that does not exist.
	const instruction = `package p

// Wire via agent.WithAttachPromptBroker so the registrant surfaces it.
type T struct{}
`
	// The shape that replaced it, wrapped the way a real record wraps —
	// the two names in different paragraphs, which is what breaks a
	// line-by-line matcher.
	const record = `package p

// Wire via attachadapter.WithPromptBroker so the registrant surfaces
// it through the capability the attach server consults.
//
// No mast entrypoint wires one, and that is a decision rather than a
// gap: mast's approvals are durable parks and its gate never prompts,
// so this bridge would have nothing to bridge.
//
// (The option this comment named through v0.8 —
// agent.WithAttachPromptBroker — never existed in this repo.)
type T struct{}
`
	// Two comment groups, one name each. The record has to be where the
	// reader is: someone who lands on the second group is owed the live
	// spelling there, not forty lines up in a different declaration.
	const splitAcrossGroups = `package p

// attachadapter.WithPromptBroker is the option.
type T struct{}

// Wire via agent.WithAttachPromptBroker.
type U struct{}
`
	// Wrapped mid-identifier. A human did this by hand; gofmt will not
	// undo it, and a matcher that only collapses newlines to spaces
	// misses it.
	const wrappedName = `package p

// Wire via agent.
// WithAttachPromptBroker so the registrant surfaces it.
type T struct{}
`
	// The ordinary case: the live name, alone, doing its job.
	const liveOnly = `package p

// Wire via attachadapter.WithPromptBroker.
type T struct{}
`
	const unrelated = `package p

// The attach prompt broker has no option at all.
type T struct{}
`
	for _, tc := range []struct {
		name      string
		src       string
		want      int
		sightings int
	}{
		{"instruction", instruction, 1, 0},
		{"record", record, 0, 1},
		{"split across groups", splitAcrossGroups, 1, 1},
		{"wrapped name", wrappedName, 1, 0},
		{"live only", liveOnly, 0, 1},
		{"unrelated", unrelated, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, seen := renamedOptionProblems(tc.name+".go", tc.src)
			if len(got) != tc.want {
				t.Errorf("got %d problems, want %d: %v", len(got), tc.want, got)
			}
			if seen["agent.WithAttachPromptBroker"] != tc.sightings {
				t.Errorf("counted %d sightings of the live spelling, want %d",
					seen["agent.WithAttachPromptBroker"], tc.sightings)
			}
		})
	}

	// Whoever trips this is usually mid-port and does not know why the
	// comment they just copied is a problem, so the message has to carry
	// the way out and the reason it recurs.
	got, _ := renamedOptionProblems("x.go", instruction)
	for _, want := range []string{
		"attachadapter.WithPromptBroker",
		"pkg/attach/prompter.go",
		"docs/sibling-sync.md",
		"#364",
		"Wire via agent.WithAttachPromptBroker",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the failure message does not mention %q:\n%s", want, got[0])
		}
	}
}

// The record in pkg/attach/prompter.go is the reason this check has to
// accept anything at all, and it is one port away from being rewritten.
// Pin it: if it goes, the check still passes, and nothing else would
// notice that the v0.8 spelling stopped being explained anywhere.
func TestThePrompterRecordStillExplainsTheOldName(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("pkg", "attach", "prompter.go"))
	if err != nil {
		t.Fatalf("read prompter.go: %v", err)
	}
	if !strings.Contains(string(src), "agent.WithAttachPromptBroker") {
		t.Error("pkg/attach/prompter.go no longer records the v0.8 spelling.\n" +
			"\tIf that was deliberate — the name is old enough that nobody is\n" +
			"\towed it any more — delete the renamedOptions row in the same\n" +
			"\tchange, because it now guards a mention that is not in the tree.\n" +
			"\tIf it was a port overwriting the file, restore the record: see\n" +
			"\tdocs/sibling-sync.md (`532f7c75`).")
	}
}
