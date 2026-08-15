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

// Command kubemcp is a two-tool stdio MCP server over a REAL Kubernetes
// cluster, used by the live acceptance tier (scripts/live-kind-v0.4.sh).
//
// The offline UAT harness proves the change-set grant mechanism against a
// file the harness itself writes. That leaves two things it cannot say:
// that a precondition survives contact with a real read tool's real JSON
// (ADK wraps an MCP server's structured content under "output", and a
// declared field path has to be right about the shape underneath), and
// that a grant is voided when a *person with kubectl* moves the object
// between an operator's approval and the call it authorized. Those need a
// cluster, so this fixture drives one:
//
//   - get_deployment(deployment) -> {deployment, namespace, replicas},
//     read-only. Structured, because that is the half a precondition can
//     be declared against.
//   - scale_deployment(deployment, replicas), mutating. The write an
//     operator approves.
//
// # Why this fixture is paranoid
//
// scale_deployment writes to whatever cluster it is pointed at. The rule
// this fixture is built to obey is the one the sibling core-sre-agent
// project arrived at the same way: a fault injector must never touch a
// real cluster, and the pin has to be mechanical rather than advisory.
// So this process refuses to start unless all of the following hold:
//
//  1. MAST_LIVE_KUBECONFIG names an existing file, and it describes
//     EXACTLY ONE context. A single-context kubeconfig is what makes
//     every other guard redundant: no bug downstream can reach a cluster
//     that is not described in the only file this process can read.
//  2. MAST_LIVE_CONTEXT names that context, and it carries LivePrefix —
//     the name only scripts/live-kind-v0.4.sh's throwaway clusters have.
//  3. Every kubectl invocation passes both --kubeconfig and --context,
//     from an environment built from scratch, so an inherited KUBECONFIG
//     cannot widen what is visible.
//
// The ambient current-context is never resolved, on any path.
//
// # Holding a call open
//
// Like testdata/uat/blocker, a call can be held: with MAST_LIVE_DIR set,
// scale_deployment writes "<tool>.started" and waits for "<tool>.release"
// before it does anything. That is the deterministic window the drift leg
// needs — the harness moves the cluster while an approved call is in
// flight, rather than sleeping and hoping.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// LivePrefix marks a context as one the live harness created. It is the
// same shape of guard core-sre-agent's kindcluster.NamePrefix is, for the
// same reason, and it is checked here as well as in the script because
// the binary is the thing holding the write.
const LivePrefix = "kind-mast-live-"

// pollInterval bounds how often a held call re-checks for its release.
const pollInterval = 50 * time.Millisecond

// kubectlTimeout bounds one kubectl invocation. A local kind cluster
// answers in milliseconds; ten seconds means the API server is gone, and
// a read that hangs would hold a turn open indefinitely.
const kubectlTimeout = 10 * time.Second

type cluster struct {
	kubeconfig string
	context    string
	namespace  string
	// dir is the coordination directory, or empty when no call is held.
	dir string
}

func main() {
	c, err := configure()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubemcp:", err)
		os.Exit(2)
	}

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mast-live-kube", Version: "0.0.1"}, nil)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "get_deployment", Description: "Read a Deployment's declared replica count."},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in getArgs) (*mcpsdk.CallToolResult, deploymentOut, error) {
			out, err := c.get(ctx, in.Deployment)
			if err != nil {
				return nil, deploymentOut{}, err
			}
			return textResult(fmt.Sprintf("%s/%s: replicas=%d", c.namespace, out.Deployment, out.Replicas)), out, nil
		})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "scale_deployment", Description: "Set a Deployment's replica count."},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in scaleArgs) (*mcpsdk.CallToolResult, deploymentOut, error) {
			if err := c.hold(ctx, "scale_deployment", fmt.Sprintf("%s=%d", in.Deployment, in.Replicas)); err != nil {
				return nil, deploymentOut{}, err
			}
			if err := c.scale(ctx, in.Deployment, in.Replicas); err != nil {
				return nil, deploymentOut{}, err
			}
			return textResult(fmt.Sprintf("scaled %s/%s to %d", c.namespace, in.Deployment, in.Replicas)),
				deploymentOut{Deployment: in.Deployment, Namespace: c.namespace, Replicas: in.Replicas}, nil
		})

	if err := srv.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

type getArgs struct {
	Deployment string `json:"deployment" jsonschema:"the Deployment's name"`
}

type scaleArgs struct {
	Deployment string `json:"deployment" jsonschema:"the Deployment's name"`
	Replicas   int    `json:"replicas" jsonschema:"the replica count to scale to"`
}

// deploymentOut is the structured result. Replicas is the DECLARED count
// (.spec.replicas), not the ready one: the live legs declare a
// precondition over this field, and a value that settles asynchronously
// would report drift every time a pod came up.
type deploymentOut struct {
	Deployment string `json:"deployment" jsonschema:"the Deployment's name"`
	Namespace  string `json:"namespace" jsonschema:"the Deployment's namespace"`
	Replicas   int    `json:"replicas" jsonschema:"the declared replica count"`
}

func textResult(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}
}

// configure reads the pin out of the environment and refuses anything
// that is not demonstrably a throwaway cluster. Every check here is
// fail-closed: this process would rather not start than write to a
// cluster it cannot prove is disposable.
func configure() (*cluster, error) {
	kubeconfig := os.Getenv("MAST_LIVE_KUBECONFIG")
	kubeContext := os.Getenv("MAST_LIVE_CONTEXT")
	if kubeconfig == "" || kubeContext == "" {
		return nil, fmt.Errorf("MAST_LIVE_KUBECONFIG and MAST_LIVE_CONTEXT are both required — " +
			"this fixture never resolves the ambient current-context")
	}
	if !strings.HasPrefix(kubeContext, LivePrefix) {
		return nil, fmt.Errorf("context %q does not start with %q — this fixture only writes to clusters "+
			"scripts/live-kind-v0.4.sh created", kubeContext, LivePrefix)
	}
	raw, err := os.ReadFile(kubeconfig) // #nosec G304 -- the harness's own path, checked below
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", kubeconfig, err)
	}
	text := string(raw)
	if n := strings.Count(text, "- context:"); n != 1 {
		return nil, fmt.Errorf("%s describes %d contexts, want exactly 1 — a merged kubeconfig puts "+
			"other clusters within reach of one wrong flag", kubeconfig, n)
	}
	if !strings.Contains(text, kubeContext) {
		return nil, fmt.Errorf("%s does not name context %q", kubeconfig, kubeContext)
	}
	ns := os.Getenv("MAST_LIVE_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return &cluster{kubeconfig: kubeconfig, context: kubeContext, namespace: ns, dir: os.Getenv("MAST_LIVE_DIR")}, nil
}

func (c *cluster) get(ctx context.Context, name string) (deploymentOut, error) {
	if strings.TrimSpace(name) == "" {
		return deploymentOut{}, fmt.Errorf("get_deployment: deployment is required")
	}
	out, err := c.kubectl(ctx, "get", "deployment", name, "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		return deploymentOut{}, err
	}
	var replicas int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &replicas); err != nil {
		return deploymentOut{}, fmt.Errorf("get_deployment %s: replica count %q is not a number: %w", name, out, err)
	}
	return deploymentOut{Deployment: name, Namespace: c.namespace, Replicas: replicas}, nil
}

func (c *cluster) scale(ctx context.Context, name string, replicas int) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("scale_deployment: deployment is required")
	}
	if replicas < 0 || replicas > 10 {
		// A fixture ceiling, not a policy: the live tier runs on one kind
		// node, and a typo'd 1000 would wedge the machine rather than fail
		// the leg.
		return fmt.Errorf("scale_deployment: replicas=%d is outside the fixture's 0..10 range", replicas)
	}
	_, err := c.kubectl(ctx, "scale", "deployment/"+name, fmt.Sprintf("--replicas=%d", replicas))
	return err
}

// kubectl runs against this cluster and only this cluster: --kubeconfig
// and --context on every invocation, in an environment built from
// scratch so an inherited KUBECONFIG cannot merge other clusters in.
func (c *cluster) kubectl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, kubectlTimeout)
	defer cancel()

	full := append([]string{
		"--kubeconfig", c.kubeconfig,
		"--context", c.context,
		"-n", c.namespace,
	}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...) // #nosec G204 -- args are the fixture's own, over a pinned throwaway context
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// hold announces dispatch and blocks until released, when the harness has
// asked for a window. Without MAST_LIVE_DIR it is a no-op, which is what
// every leg that is not about timing wants.
//
// The ledger line records which call ran with which arguments — the same
// evidence testdata/uat/blocker's ledger carries, and the reason a leg can
// say "the second call never fired" rather than "the count looks right".
func (c *cluster) hold(ctx context.Context, tool, detail string) error {
	if c.dir == "" {
		return nil
	}
	if f, err := os.OpenFile(filepath.Join(c.dir, tool+".calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = fmt.Fprintf(f, "%s %s\n", tool, detail)
		_ = f.Close()
	}
	release := filepath.Join(c.dir, tool+".release")
	if _, err := os.Stat(release); err == nil {
		return nil
	}
	if f, err := os.Create(filepath.Join(c.dir, tool+".started")); err == nil {
		_ = f.Close()
	}
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		if _, err := os.Stat(release); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
