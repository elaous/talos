# Security policy

## Supported versions

talos has no tagged releases yet. Security fixes MUST target the `main` branch only.

## Reporting a vulnerability

Reporters MUST NOT open a public issue or pull request for a vulnerability.

Use GitHub private vulnerability reporting: open the repository's **Security** tab, choose
**Report a vulnerability**, and describe the problem. This creates a private security advisory visible only to
the maintainers and you.

A report SHOULD include:

- the affected version or commit;
- steps to reproduce, or a proof of concept;
- the impact you expect (for example: reading files outside the PDF library, reaching a private network
  address through `fetch_url`, bypassing the `Host` or `Origin` checks).

The maintainers will try to acknowledge a report within 7 days and to agree on a fix and disclosure date with you.
Reporters SHOULD allow time for a fix before disclosing publicly.

## Scope

In scope: the talos server, its web fetching, its PDF handling and its container image.

Out of scope: the fact that talos has no login (documented in the README: it MUST be put behind an
authenticating proxy before it is exposed beyond loopback), and vulnerabilities in the model servers
(Ollama or others) or in the `gojiberry` module (https://github.com/elaous/gojiberry), which SHOULD be reported to their own maintainers.
