---
name: adr
description: >
  Write a new Architecture Decision Record (ADR) file in the Prometheus proposal
  style: Status Quo → Goal → Non-Goal → Proposed Solution → Alternatives.
  Use this skill whenever the user asks to write, add, create, or record an ADR,
  architecture decision, design decision, proposal, or RFC — even if they say
  things like "document why we chose X", "record the decision about Y", "add an
  ADR for the container approach", or "we should write up why we did Z".
  Also trigger when the user asks to "document this decision" or "write up the
  rationale" for any architectural or design choice in the codebase.
---

# Write ADR

You are writing an Architecture Decision Record (ADR) in the Prometheus proposal
style. This format is more narrative and complete than the terse Nygard style —
it tells the whole story: where we were, where we're going, what's out of scope,
the chosen solution with full rationale, and the roads not taken.

## Step 1 — Gather context

Before writing anything, determine what you already know from the conversation:

- **Title**: A short noun phrase for the decision (e.g. "Direct VM SSH instead of OrbStack multiplexer")
- **Status Quo**: What is the current state that made this decision necessary?
- **Goal**: What does this decision achieve? What problem does it solve?
- **Non-Goal**: What is explicitly out of scope? (What might people expect this to address but doesn't?)
- **Proposed Solution**: The chosen approach — what was decided and why?
- **Alternatives**: Other options that were seriously considered but rejected, and the reason each was ruled out.

If any of these are missing or unclear from the conversation, ask the user — but group your questions into one message rather than asking one at a time. The more context already in the conversation, the less you need to ask.

## Step 2 — Find the next ADR number

Look for an existing ADR directory. Common locations (check in order):
- `docs/adr/`
- `docs/decisions/`
- `adr/`
- `decisions/`

List the files and find the highest numbered ADR. The new one gets the next number (zero-padded to three digits, e.g. `003`). If there are no existing ADRs, start at `001`.

If you can't find any ADR directory, ask the user where they keep them before creating a new directory.

## Step 3 — Write the ADR

Create the file at `<adr-dir>/ADR-<NNN>-<kebab-case-title>.md`.

Use this exact structure:

```markdown
# ADR-<NNN>: <Title>

**Status:** <Accepted | Proposed | Deprecated | Superseded by ADR-NNN>
**Date:** <YYYY-MM>
**Owners:** <name or team, if known — omit line if not>
**Related:** <issue/PR links if any — omit line if not>

## Status Quo

<Describe the current state before this decision. What exists today? What
problem or gap motivated this ADR? Be specific — name the concrete system,
library, or behavior that was the starting point. This section answers:
"What is the world like right now, and why isn't it good enough?">

## Goal

<What will be true after this decision is implemented? State it as a positive
outcome. One to three sentences — be concrete, not vague ("confidentiality,
integrity and client authenticity" is better than "more secure").>

## Non-Goal

<What is explicitly out of scope? List things that readers might expect this
decision to address but that are deliberately deferred or left to a different
ADR. If there are genuinely no non-goals, write "None identified." rather than
leaving the section empty.>

## Proposed Solution

<The full story of what was decided and why. This is the most important section.
Structure it as: what the solution is, then why it is correct. Use bold lead-ins
for each reason ("**Mutual TLS is already standard in the Prometheus ecosystem.**")
Include technical specifics — library names, flag names, protocol details — so a
reader can implement from this document alone.>

## Alternatives

<For each seriously-considered alternative, a sub-section:

### <Alternative name>

Why it was a real option, and why it was ultimately rejected. One to three
sentences each. "We didn't think of X" is not an alternative — only options
that were genuinely evaluated belong here.>
` ` `

## Style guidance

**Prose over bullets.** Each section should read as connected paragraphs, not
a list of disconnected points. Reserve bullet lists for genuinely enumerable
items (a list of three alternatives, a set of flags).

**Name the specific things.** Vague rationale ("it's more secure") is less
useful than specific rationale ("AES-GCM's 16-byte auth tag detects any
tampering before plaintext is returned"). Name libraries, file paths, flag
names, error types where relevant.

**The Non-Goal section earns its place.** It's not filler — it tells readers
what *not* to expect and often prevents future scope creep. A good non-goal is
something that someone reading the goal section might naturally assume is in scope.

**Alternatives must be real contenders.** If an alternative was never seriously
considered, don't list it. The alternatives section exists to show due diligence
and help future readers who ask "why didn't you just use X?"

**Cross-reference other ADRs.** If this decision builds on or is constrained by
a previous ADR, say so inline (e.g. "see ADR-001"). Don't just list ADR numbers
in a table — reference them where the dependency actually matters in the prose.

## After writing

Tell the user:
1. The file path created
2. A one-sentence summary of what the ADR records
3. Whether any section felt thin (e.g. "the Non-Goal section is minimal — let me
   know if there's something that should be explicitly excluded")

Do not add the ADR to any index or README unless the user asks. Do not create
more than one ADR per request unless the user explicitly asks for multiple.