# scripts/

Mechanical enforcement for the documentation rules in `CLAUDE.md` section 4.

- `check-ascii.mjs` -- fails on any non-ASCII character, naming the offender and its
  ASCII replacement. Enforces rule 4.1.
- `check-mermaid.mjs` -- parses every ```mermaid block with Mermaid's own parser under
  jsdom. Enforces rule 4.2, and catches the failure mode a VitePress build cannot:
  diagrams render client-side, so a malformed one builds clean and breaks in the reader's
  browser.

Run both plus the build with `npm run docs:check`.
