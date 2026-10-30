# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-10-30

The first release. ghapp is an MCP server that works on GitHub as one
GitHub App, behind Straza.

### Added

- 15 tools: `repo_list`, `repo_get`, `branch_list`, `file_get`, `pr_list`,
  `pr_get`, `pr_create`, `pr_comment`, `pr_merge`, `issue_list`,
  `issue_get`, `issue_create`, `issue_comment`, `search_code` and
  `search_issues`. Each carries the `ai.straza/object` mark and one
  required `repo`, and `repo_list` reports repo ids and GitHub custom
  properties for Straza's listing.
- One installation token per call, for that one repo and only the tool's
  permissions, reused per caller, repo and permission set until 5 minutes
  before it expires. Names are taken exactly as GitHub spells them,
  rename redirects are refused, and the repo id Straza approved is
  checked. Refusals are tool results with one sentence and a code.
- Who may call: an OIDC token from a listed client, Straza's gateway
  secret, and a listing token that opens `repo_list` only. `--http` needs
  one of them. `--dev` serves stdio, or loopback without caller
  authentication, for development and tests.
- Git through Straza's holder: `/git` takes a Straza grant with the
  holder's DPoP proof, as Straza's grant spec `spec/grant` says, pushes
  exactly the granted branch updates and nothing else, and streams git to
  GitHub with a token that never leaves ghapp. `git_fetch` and `git_push`
  check the repo and give the plain git command.
- An audit log of JSON lines: every token minted with GitHub's
  `hashed_token`, every tool call and every git request, never a token or
  a secret. A footer names the agent on pull requests, issues and
  comments, unless `GHAPP_MCP_AGENT_FOOTER=false`.
- `GHAPP_MCP_READ_ONLY=true` leaves out the tools that write and refuses
  every push.
- A container image for linux/amd64 and linux/arm64, release binaries for
  Linux, macOS and Windows with checksums, SBOMs and signed provenance, and
  `./demo/run.sh`, which runs the whole flow offline against stand-ins for
  GitHub, Straza and its holder.

Dependencies: the official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`),
`github.com/go-jose/go-jose/v4` for the App's JWT and Straza's grants, and
`github.com/coreos/go-oidc/v3` with `golang.org/x/oauth2` for OIDC callers.

[Unreleased]: https://github.com/strazahq/ghapp-mcp-server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/strazahq/ghapp-mcp-server/releases/tag/v0.1.0
