# Security policy

## Reporting a vulnerability

Please report security problems privately through GitHub: open the repository's
**Security** tab and choose **Report a vulnerability**. Do not open a public issue.

Include what you found, how to reproduce it, and the output of `ragloop version`.
You should get an acknowledgement within a week.

Issues in scope include prompt-injection bypasses that reach the model or the
answer, retrieval of documents outside a user's groups, and leaks of screened text.

## Supported versions

Only the latest release receives fixes.

## Verifying a release

Every release binary has a signed build provenance attestation. With the GitHub CLI:

```sh
gh attestation verify ragloop_<version>_<os>_<arch>.tar.gz --repo <owner>/ragloop
```
