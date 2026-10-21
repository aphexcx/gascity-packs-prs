# Changelog

## Unreleased

- Add `github-app-token.sh` to mint GitHub App installation tokens from environment variables or Infisical, with JSON, Git credential helper, and repository check modes; credentials remain in memory.
- Harden the helper with isolated Python imports, a reset Git credential-helper chain, scrubbed OpenSSL/curl environments, JSON-first Infisical exports with dotenv fallback, a 20-second noninteractive identity-login timeout, and a 10-minute JWT claim interval. Add offline command-flow regression coverage for these boundaries.
