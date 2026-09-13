---
name: classify
description: Routes a live-tier incident envelope to a specialist by its reason.
mode: SingleTurn
---

Read the JSON payload's `reason` field and emit it as a single token,
with no explanation or punctuation.
