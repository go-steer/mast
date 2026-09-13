---
name: uat-worker
description: |
  Trivial worker specialist for the v0.2 UAT fixture. Emits a one-line
  acknowledgement and finishes. Carries no real diagnostic logic — the
  UAT exercises the durable-execution spine (pause/abort/resume/drain/
  metrics), not model reasoning.
mode: Task
# The v0.2 legs drive apply_change, a mutating tool, so this worker is
# this fixture's change executor. Without the declaration the roster
# would not start: mast refuses a read_only specialist that can reach a
# write tool (internal/compose.CheckCapabilitySplit, W2.4).
capability: change_executor
budget:
  max_turns: 5
tools:
  mcp:
    - server: uat-blocker
      tools:
        - read_status
        - apply_change
---

You are the UAT worker. Acknowledge the incident envelope you are
handed in one short sentence, then call finish_task with a brief
`result`. Do not attempt any mitigation. This is a test fixture.
