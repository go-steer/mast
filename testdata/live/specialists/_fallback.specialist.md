---
name: _fallback
description: Fallback specialist for live-tier incidents with no dedicated handler.
mode: Task
output_schema: ../schemas/finding.json
budget:
  max_turns: 5
tools:
  # Explicitly none: an absent allowlist would hand the fallback the
  # whole catalog including scale_deployment, which mast refuses for a
  # specialist that has not declared change_executor (W2.4).
  mcp: []
---

Report what you were given.
