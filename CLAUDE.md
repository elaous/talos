# Global rules

## Comments, docs and commits
Code explains itself through names and structure. A comment is allowed only where the code needs clarification: behavior a careful reader would not expect, or a technical constraint a refactor must keep. Keep it strictly technical, about ten words or fewer. Never write narration in source code: no story of the bug or the change, no "why this was added", no issue, ticket or PR references, unless the user asks for them. That context goes in the PR description. Write longer explanations of code in a separate Markdown file that mirrors the source path only when the user asks for one.

Commit subjects are imperative. Add a body only for a fact the diff cannot show.

In documentation, plans and PR descriptions, state obligations with RFC 2119 keywords (MUST, SHOULD, MAY); do not use them in code comments or commit subjects.

## Model selection
- Sessions MUST start on Sonnet 5.5 (`/model sonnet`).
- Haiku 5.5 SHOULD be used for: search, file lookup, renames, formatting, boilerplate, simple test generation, log or diff summaries, and read-only subagents.
- Sonnet 5.5 SHOULD be used for: features, bug fixes, refactors within one module, test writing, code review, and routine debugging.
- Opus 5.5 SHOULD be used for: architecture and API design, cross-module refactors, concurrency, security-sensitive code, migrations, and bugs unresolved after two Sonnet attempts.
- Fable 5.1 MAY be used only for long projects that exceed a single session.
- Start one tier lower than expected and escalate only when the result is weak, never for style preferences.
- Subagents MUST NOT inherit Opus by default. Set `model: haiku` or `model: sonnet` in the agent definition, and use Opus only for design review.
- Each task MUST get its own session. Use `/clear` between unrelated tasks and `/compact` before a session passes about half its context.

## Skills
- A procedure of 3 or more steps done twice in a project SHOULD become a skill in `.claude/skills/`, or in `~/.claude/skills/` if shared across projects. Claude MUST NOT create skills for one-off tasks, and MAY mention the opportunity at the end of a task.
- Deterministic steps (formatting, linting, file generation, validation) MUST be a script bundled with the skill.
- Each skill MUST cover one workflow, have a specific description stating when it applies, and keep SKILL.md under 500 lines.

## PRs
1. After creating a PR, Claude MUST run a fresh, independent review of it in a new subagent that has none of the authoring context.
2. Claude MUST address all review feedback, leaving no loose ends: fix each finding, or reply on its thread with the reason it stays as is.
3. Claude MUST merge the PR once it is clean: all review findings addressed, the repo's tests pass when run locally, and no merge conflict. A CI failure the PR causes blocks the merge; one outside the PR, such as no runner being assigned, does not. Claude MUST NOT merge when the user says otherwise.
