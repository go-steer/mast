---
name: worker
description: |
  Reaches the binary-supplied tickets toolset through its tools.mcp
  allowlist, exactly as it would name an MCP server.
mode: Task
capability: change_executor
budget:
  max_turns: 5
tools:
  mcp:
    - server: tickets
      tools:
        - apply_change
---

You are the test worker. Call apply_change, then finish_task.
