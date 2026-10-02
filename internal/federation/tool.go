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

package federation

import (
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// InvokeRemoteAgentArgs is the LLM-facing argument schema for the
// invoke_remote_agent planner tool (docs/federation-design.md: "the
// planner's tool vocabulary gains one class").
type InvokeRemoteAgentArgs struct {
	// Reference identifies the remote agent:
	// <scheme>://<name>[/<skill>], e.g. a2a://external-triage/investigate-incident.
	Reference string `json:"reference"`

	// Inputs is the structured input payload for the remote agent.
	Inputs map[string]any `json:"inputs,omitempty"`
}

// NewInvokeRemoteAgentTool builds the single unified planner tool
// `invoke_remote_agent(reference, inputs)` over a Registry. cmd/mast
// (or a library consumer) constructs the Registry with the adapters the
// deployment permits and adds the returned tool to the planner's tool
// list; this package does no global registration.
//
// v0.1 semantics: the call blocks to a bounded timeout (the remote
// agent config's timeout; docs/a2a-design.md v0.1 phasing row) and the
// tool result carries only the terminal state — intermediate events
// and HITL propagation arrive with v0.2 per the frozen Handle contract.
func NewInvokeRemoteAgentTool(reg *Registry) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "invoke_remote_agent",
		Description: "Dispatch a task to a remote agent and return its terminal result. " +
			"The reference selects the agent: a2a://<name>/<skill> invokes skill <skill> " +
			"on the configured A2A agent <name>. Blocks until the remote task reaches a " +
			"terminal state or the configured timeout expires.",
	}, func(ictx adkagent.Context, args InvokeRemoteAgentArgs) (map[string]any, error) {
		// adkagent.Context embeds context.Context, so the invocation's
		// cancellation and deadlines propagate into the adapter (and,
		// for the A2A adapter, into remote tasks/cancel on abort).
		h, err := reg.Invoke(ictx, args.Reference, args.Inputs, InvokeOptions{})
		if err != nil {
			return nil, err
		}
		res, err := h.Wait(ictx)
		if err != nil {
			return nil, err
		}
		out := map[string]any{
			"state": res.State,
		}
		if res.RemoteID != "" {
			out["task_id"] = res.RemoteID
		}
		if res.Text != "" {
			out["text"] = res.Text
		}
		if len(res.Output) > 0 {
			out["output"] = res.Output
		}
		return out, nil
	})
}
