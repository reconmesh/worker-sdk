---
name: Bug report
about: Something that should work doesn't
labels: bug
---

## What's broken

<!-- One paragraph: what you expected vs what happened. -->

## Reproduction

<!-- The smallest possible repro. Examples:
     - `docker compose up`, then …
     - `make up && curl -X POST …`
     - paste a manifest.yaml that fails validation
-->

## Logs / output

<!-- The exact error. Wrap in ``` for fixed-width. Trim to the
     relevant lines. -->

```
<paste here>
```

## Versions

- worker-sdk: <sha or tag>
- controlplane: <sha or tag>
- module / worker affected: <name + tag>
- OS / Docker: <e.g. Linux 6.1, Docker 27>


<!-- If this is regression on a previously-shipped item,
     name it. Helps triage decide whether it's a recent shipment
     issue or a long-standing gap. -->
