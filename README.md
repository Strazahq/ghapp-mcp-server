# ghapp-mcp-server

ghapp is the GitHub server for [Straza](https://straza.ai). It lets AI
agents work on GitHub through MCP as one GitHub App: each call reaches one
repo, with only the permissions its tool needs, and no GitHub token ever
sits on the agent's machine.

> **ghapp is of no use without Straza.** Straza decides which repos each
> agent may touch, and brings the grant that opens git. Without it, ghapp
> would reach every repo the App is installed on, and git would not work
> at all. If you don't run Straza, use GitHub's official MCP server,
> [github/github-mcp-server](https://github.com/github/github-mcp-server),
> instead.

![Who holds what. Four parts, left to right: the agent machine, Straza, ghapp and GitHub. The agent machine never holds a GitHub token. ghapp holds the App key and the tokens. One call goes through Straza and ghapp to GitHub and back.](docs/diagrams/overview.svg)

**MCP** is the protocol AI agents use to call tools. **Straza** is the
gateway in front of MCP servers: its roles say which tools and repos an
agent may use. **ghapp** holds the GitHub App's private key and makes a
short-lived GitHub token for each call. **GitHub** sees every action as
`ghapp[bot]`.

## A tool call

![One tool call. joe-macos-agent calls pr_create on Strazahq/pay-api. Straza checks a role covers the repo, ghapp checks the caller, the name and the id, makes a token for repo 812345678 with pull_requests write and contents read, and GitHub creates the PR. The same call on Strazahq/straza is refused at Straza.](docs/diagrams/tool-call.svg)

Two locks. Straza decides whether the agent's role covers the repo, and a
repo no role covers stops there. ghapp then makes sure the call can touch
only that repo: it checks who is calling, that the name is exact and that
the repo id is the approved one, and makes a token for that one repo with
only the tool's permissions.

## git push

![git push through Straza's holder. The agent types plain git push; Straza's holder on the same machine, under its own OS account, reads the branch and commits, gets a grant from Straza for exactly those, and sends the push to ghapp with the grant and a proof signed by its key. ghapp checks both and pushes to GitHub with a token that stays in ghapp.](docs/diagrams/git-grant.svg)

The agent types plain `git push`. Straza's holder, a service on the same
machine under its own OS account, gets a grant for exactly that branch and
those commits, and sends the push to ghapp with the grant and a proof
signed by its own key. ghapp checks both, compares the push with the
grant, and streams it to GitHub with a token that stays in ghapp. A push
can be 2 GiB, so the code never goes through MCP.

Over MCP, `git_push` and `git_fetch` only check the repo and give the plain
git command. Only the holder's grant opens git.

## Tools

`repo_list`, `repo_get`, `branch_list`, `file_get`, `pr_list`, `pr_get`,
`pr_create`, `pr_comment`, `pr_merge`, `issue_list`, `issue_get`,
`issue_create`, `issue_comment`, `search_code` and `search_issues`, and
for git `git_fetch` and `git_push`. Each names one repo as `owner/name`,
exactly as GitHub spells it. `GHAPP_MCP_READ_ONLY=true` leaves out the
tools that write.

## Install

The container image, for linux/amd64 and linux/arm64:

```sh
docker pull ghcr.io/strazahq/ghapp-mcp-server:0.1.0
```

A release binary for Linux, macOS or Windows, checked against the release's
checksums and its signed build provenance:

```sh
v=0.1.0
f=ghapp-mcp-server_${v}_linux_amd64.tar.gz
base=https://github.com/strazahq/ghapp-mcp-server/releases/download/v$v
curl -fsSLO "$base/$f" -O "$base/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify "$f" --owner strazahq
tar -xzf "$f"
```

Each archive also has a signed SPDX SBOM:
`gh attestation verify "$f" --owner strazahq --predicate-type https://spdx.dev/Document/v2.3`.

With Go:

```sh
go install github.com/strazahq/ghapp-mcp-server@v0.1.0
```

Then create the GitHub App: [docs/github-app.md](docs/github-app.md) lists
the permissions each tool needs.

## Settings

Run `ghapp-mcp-server --http 0.0.0.0:8080` for HTTP at `/mcp`, with the
gateway secret or OIDC set, so that only Straza calls it. `--dev` is for
development and tests only: it serves stdio, or `--http` on loopback
without caller authentication.

```sh
docker run -p 8080:8080 \
  -e GHAPP_APP_ID=123456 \
  -e GHAPP_PRIVATE_KEY_FILE=/run/secrets/app-key.pem \
  -e GHAPP_MCP_GATEWAY_SECRET="$(cat gateway-secret)" \
  -v "$PWD/app-key.pem:/run/secrets/app-key.pem:ro" \
  ghcr.io/strazahq/ghapp-mcp-server:0.1.0 --http 0.0.0.0:8080
```

| Setting | What it does |
|---|---|
| `GHAPP_APP_ID` | The GitHub App's id. |
| `GHAPP_PRIVATE_KEY` or `GHAPP_PRIVATE_KEY_FILE` | The App's private key, as PEM or as a path. It never leaves the server. |
| `GHAPP_API_URL` | GitHub's API. Default `https://api.github.com`. |
| `GHAPP_MCP_OIDC_ISSUER`, `GHAPP_MCP_OIDC_AUDIENCE`, `GHAPP_MCP_OIDC_CLIENTS` | Accept tokens from this identity provider, with this audience, from these clients only. |
| `GHAPP_MCP_GATEWAY_SECRET` | A bearer only the gateway holds, for setups without client credentials. ghapp's log then cannot name the agent. |
| `GHAPP_MCP_LISTING_TOKEN` | Opens `repo_list` and nothing else, for Straza's listing of repos. |
| `GHAPP_MCP_ATTRIBUTES` | Which GitHub custom properties `repo_list` reports, such as `team`. |
| `GHAPP_MCP_READ_ONLY` | `true` leaves the write tools out. |
| `GHAPP_PUBLIC_URL` | The address Straza's holder reaches ghapp's git endpoint at. Turns on git transport and the git tools, and needs `GHAPP_GIT_GRANT_ISSUER`. |
| `GHAPP_GIT_GRANT_ISSUER` | Straza's public URL, the issuer of the grants the git endpoint takes. |
| `GHAPP_MCP_AGENT_FOOTER` | `false` leaves out the line naming the agent on PRs, issues and comments. |

Also `GHAPP_MCP_OIDC_DISCOVERY_URL`, `GHAPP_MCP_OIDC_CLIENT_CLAIM` (default
`azp`), `GHAPP_GIT_UPSTREAM` (default `https://github.com`),
`GHAPP_GIT_MAX_PUSH_BYTES` (default 2 GiB), `GHAPP_GIT_TIMEOUT` and
`GHAPP_GIT_GRANT_JWKS_URL` (default the issuer's
`/.well-known/straza/jwks.json`).

A grant at `/git` is spent once: a push grant by its push, a fetch grant
when GitHub starts sending a pack. ghapp keeps spent grants in memory, per
instance. Behind a load balancer, a grant can be used once on each
instance within its 5 minutes, and only by the holder that has its key.

## Behind Straza

Only Straza may reach ghapp, and two setup rules at the identity provider
make the token prove it:

1. Every client that can get a token with ghapp's audience signs in with
   Straza's client assertion key alone: no client secret, no other key.
2. An agent's own login to Straza (`straza enroll --headless`) is a
   separate client that never carries ghapp's audience.

Without client credentials, set `GHAPP_MCP_GATEWAY_SECRET` and give Straza
the same secret. ghapp's log then cannot tell agents apart, though
Straza's audit can. Straza also needs the gateway secret on OIDC setups
while its tool-list probe carries no token. Give Straza the listing token
as the server's listing credential, never as its own secret.

## Links

- [docs/how-it-works.md](docs/how-it-works.md): the diagrams, with a few
  lines each.
- [docs/github-app.md](docs/github-app.md): create the GitHub App, and the
  permissions each tool needs.
- [demo/README.md](demo/README.md): the whole flow offline, against
  stand-ins for GitHub, Straza and its holder.
- [SECURITY.md](SECURITY.md), [CONTRIBUTING.md](CONTRIBUTING.md) and
  [CHANGELOG.md](CHANGELOG.md).

Licensed under the Apache License, Version 2.0: see [LICENSE](LICENSE)
and [NOTICE](NOTICE).
