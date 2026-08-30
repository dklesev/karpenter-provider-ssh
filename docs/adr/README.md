# Architecture decisions

The decisions that shaped this project, and — more usefully — the reasoning
that would have to change for them to be revisited.

They exist because the questions keep coming back. "Why SSH and not an agent?"
is a fair question, it has a real answer, and that answer should not have to be
reconstructed from scratch every time someone asks. Each record states what was
decided, what it cost, and what would overturn it. A decision with no stated
cost is a decision nobody examined.

These are records, not documentation: they describe the reasoning at a point in
time. When one is superseded, it gets marked superseded rather than edited — a
record that quietly changes to match the present teaches nothing.

| # | Decision | Status |
|---|---|---|
| [0001](0001-ssh-as-the-transport.md) | SSH is the transport; the shim is the API | Accepted |
| [0002](0002-api-group-and-naming.md) | API group `karpenter.dklesev.github.io`, prefix `kpssh` | Accepted |
