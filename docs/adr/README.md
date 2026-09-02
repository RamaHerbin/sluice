# Architecture Decision Records

This directory records the decisions that shape sluice and the reasoning behind them, so that a later reader (or a later wave of agents) can tell a deliberate choice from an accident. An ADR is a short, dated note. Once accepted it is not rewritten; a change of mind gets a new number and marks the old record as superseded.

The design documents in `../design/` describe the current state of the system. ADRs describe why it got there. When the two disagree, fix the design document and, if the disagreement is a real change of mind, write a new ADR.

## When to write one

- A choice that constrains several packages or is expensive to undo: process shape, model boundaries, the publication mechanism.
- Any new third-party module. `CLAUDE.md` requires an ADR plus orchestrator approval before `go.mod` changes.
- A deliberate deviation from Caddy, Traefik or the Kubernetes Ingress conventions that a reader would otherwise assume sluice follows.

Small local choices (a data structure inside one package, a test helper) belong in code comments, not here.

## Conventions

- File name `NNNN-short-title.md`, four digits, sequential, never reused.
- Status is one of `Proposed`, `Accepted`, `Superseded by ADR-NNNN`, `Deprecated`, always with a date.
- Plain English sentences. No em-dashes. No marketing. Nothing is described as production-ready.
- The Decision names the actual package, type or function it constrains (`router.Store`, `controller.Translate`) so the text can be checked against the code.
- Sources are cited with URLs. When a source could not be fetched or verified, the ADR says so instead of guessing.
- Keep an ADR under about 80 lines. If it needs more, it is probably two decisions.
- An optional `Dissent` section records an author's disagreement without changing the decision.

## Template

```markdown
# ADR NNNN: Title

Status: Accepted, YYYY-MM-DD

## Context

Two to six sentences: the forces at play and what the system needs.

## Prior art

How Caddy, Traefik, Kubernetes or the Go standard library handle this, briefly compared, with URLs.

## Decision

What sluice does, naming the package, type or function it constrains.

## Consequences

Positive:
- ...

Negative:
- ...

## Alternatives considered

- **Alternative.** What it is and what it would give. Why not for sluice now: ...

## Revisit when

- The concrete trigger that would reopen this decision.

## Dissent (optional)

A clearly marked disagreement, recorded without changing the decision.

## References

- Title: URL
```

## Index

| Number | Title | Status |
|---|---|---|
| [0001](0001-single-binary.md) | One binary, control plane and data plane in one process | Accepted, 2026-09-03 |
| [0002](0002-kubernetes-independent-model.md) | A Kubernetes-independent routing model | Accepted, 2026-09-03 |
| [0003](0003-immutable-config-atomic-swap.md) | Immutable routing table published by atomic swap | Accepted, 2026-09-03 |
| [0004](0004-ingress-before-gateway-api.md) | Ingress first, Gateway API later on the same model | Accepted, 2026-09-03 |
| [0005](0005-simple-round-robin.md) | Simple round-robin with per-table counters | Accepted, 2026-09-03 |
| [0006](0006-no-file-hot-reload.md) | No configuration file and no file hot-reload | Accepted, 2026-09-03 |
| [0007](0007-dependencies.md) | Third-party dependencies: client-go and client_golang only | Accepted, 2026-09-03 |
