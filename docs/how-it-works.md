# How ghapp works

Five pictures, one idea each. They use one example throughout: the agent
`joe-macos-agent`, the repo `Strazahq/pay-api` with GitHub id `812345678`,
and the Straza role `ghapp-write-payments` for team payments.

Words used here:

- **MCP**: the protocol AI agents use to call tools on a server.
- **Straza**: the gateway in front of MCP servers. It holds roles,
  approvals and the audit log. midPoint sees each Straza role as an
  application role (AR).
- **ghapp**: this server. It acts as a GitHub App, a bot account with its
  own private key and permissions.
- **Token**: a GitHub installation token. ghapp makes one for a set of
  repos and permissions, and it expires within an hour.
- **Holder**: Straza's service on the agent's machine, `straza service`,
  under its own OS account. It takes the agent's plain git and brings
  ghapp a grant from Straza for each request.

ghapp works only behind Straza. Without Straza, use GitHub's own MCP
server or `gh`.

Each picture says at the bottom what is built and what is not. The colours stay the same in every picture: agent machine blue,
Straza green, ghapp amber, GitHub grey.

## 1. Who holds what

![Who holds what. The agent machine holds the agent, git through Straza's holder and its Straza session, and never a GitHub token. Straza holds roles, approvals and audit. ghapp holds the App private key and the GitHub tokens, which stay in ghapp, and checks names and ids. GitHub holds the repos and its audit log.](diagrams/overview.svg)

*Each part holds only what it needs. The key and the tokens stay in ghapp.*

- The agent machine has git, Straza's holder, and the agent's own Straza
  session. It never holds a GitHub token.
- Straza holds the roles. A role names tools, and its field `covers` names
  the repos those tools may touch.
- ghapp holds the App's private key and makes the tokens: one per agent,
  repo and permission set, reused until shortly before it expires.
- GitHub shows every action as `ghapp[bot]`, whichever agent did it. Its
  audit log keeps a hash of the token used.

## 2. One tool call

![One tool call. joe-macos-agent calls pr_create on Strazahq/pay-api. 1, Straza: a role covers the repo. 2, ghapp: who is calling. 3, ghapp: exact name, same id. 4, ghapp: a token for repo 812345678 with pull_requests write and contents read. 5, GitHub: the PR is created as ghapp[bot]. The same call on Strazahq/straza is refused at Straza.](diagrams/tool-call.svg)

*Two locks: Straza checks the repo against the agent's roles, then ghapp
makes sure the call can reach only that repo.*

- Step 1 is Straza's. If no role the agent holds covers the repo, Straza
  refuses in one sentence and lists up to ten repos the roles do cover.
  ghapp and GitHub never see the call.
- Step 2: behind Straza, the caller arrives with a token from the
  identity provider, such as Keycloak. ghapp accepts it only from the
  clients in `GHAPP_MCP_OIDC_CLIENTS`.
- Step 3: the name must match GitHub's spelling byte for byte, and ghapp
  never follows a rename. When the role covers some repos, Straza also
  sends the repo id it approved. A repo deleted and created again under
  the same name has a new id, so ghapp refuses it.
- Step 4: the token reaches repo `812345678` only, with only what
  `pr_create` needs. When ghapp refuses a tool call, the agent gets a tool
  result with one sentence and a code, never an HTTP error.

## 3. git push

![git push through Straza's holder. 1: the agent types plain git push origin fix-login, and one git setting sends it to Straza's holder, a service on the same machine under its own OS account. 2: the holder reads the update before any code moves. 3: Straza checks that a role holds git_push and covers the repo, a person may approve seeing the commits, and Straza returns a grant: signed, 5 minutes, this branch and these commits. 4: the holder sends the push to ghapp with the grant and a proof signed by its key, not through Straza, and ghapp checks Straza's signature, the proof, and that the push is exactly what was granted. 5: GitHub takes the pack, with a token that stays in ghapp. The agent can read nothing that opens ghapp or GitHub.](diagrams/git-grant.svg)

*The agent types plain git. Straza decides on the branch and the commits.*

ghapp checks each grant as Straza's grant spec, `spec/grant`, says.
`./demo/run.sh` shows the whole flow with stand-ins for Straza and its
holder.

- Why beside Straza's MCP traffic: Straza reads at most 4 MiB per MCP
  message, and a push can be up to 2 GiB. ghapp streams the pack and
  never holds it in memory.
- The holder runs under its own OS account, so an agent running as the
  person cannot read its key. A grant copied off the machine opens nothing
  without that key.
- Straza checks the push as a call of `git_push` with the branch and the
  commits, so roles, covered repos, policies and approvals work as for
  any tool, and the approver sees what is pushed. Fetch and clone work the
  same way, with `git_fetch`.
- ghapp refuses a push that differs from the grant by one ref or one
  commit, and any delete or tag. The old commit in the grant makes each
  update a compare-and-swap at GitHub. It does not stop a force push:
  GitHub's branch rules still do.
- Called over MCP, `git_push` and `git_fetch` check the repo and give the
  plain git command. They hand out nothing that opens git.

## 4. What a role covers

![What a role covers. The Straza role ghapp-write-payments holds pr_create, pr_merge and git_push. Its field covers is one of three forms: where team is payments covers pay-api, pay-ledger and refunds; every covers all nine repos; objects: [Strazahq/pay-api] covers one. midPoint shows it as AR:ghapp-write-payments, covers repo where team is payments.](diagrams/scope.svg)

*One role binds tools and repos together, and `covers` says which repos.*

- `covers` takes one form per role: `every`, or a list of named repos.
  `where`, on attributes such as `team`, is designed and parked in Straza.
- `team` is a GitHub custom property. ghapp's `repo_list` hands every repo
  with its id and team to Straza, which lists them every 10 minutes. Once
  `where` is built, a new repo with team payments is covered at the next
  listing.
- If repo admins can set an attribute, any of them can move their repo
  into a role. Straza warns about that when the role is published.
- midPoint imports the role as an application role. Its `covers` line shows
  the rule in words, so a certifier sees what the role means. The list of
  repos stays in Straza, so a new listing never makes midPoint read the
  role again.
- Tools and repos sit on the same role on purpose. As two separate roles,
  read on A and write on B would add up to write on A.

## 5. Following one push

![Following one push. Three log lines. Straza audit: joe-macos-agent, git_push, Strazahq/pay-api, allowed. ghapp log: joe-macos-agent, refs/heads/fix-login, Strazahq/pay-api, token hash. GitHub audit log: ghapp[bot], git.push, Strazahq/pay-api, hashed_token. The same hash links GitHub's line to ghapp's, and the same agent links ghapp's line to Straza's.](diagrams/audit.svg)

*GitHub sees one bot. The token hash tells the agents apart.*

- GitHub records `hashed_token`, the SHA-256 of the token, for GitHub App
  tokens too. ghapp's log records the same hash next to
  the agent, so each GitHub event leads back to one agent. That only holds
  if agents never share a token.
- GitHub keeps git events for 7 days, and shows them only in exports,
  the API and streaming.
- With the static gateway secret, ghapp does not know the agent, and its
  log has only the repo and the time. Straza's audit still names the
  agent.

## What is built

- 15 typed tools: `repo_get`, `branch_list`, `file_get`, `pr_list`,
  `pr_get`, `pr_create`, `pr_comment`, `pr_merge`, `issue_list`,
  `issue_get`, `issue_create`, `issue_comment`, `search_code`,
  `search_issues`, `repo_list`, and `git_fetch` and `git_push` for git.
- ghapp's checks: exact names, no rename redirects, the repo id from
  Straza, a token for one repo and the tool's permissions, refusals as
  tool results with a code.
- One token per agent, repo and permission set, reused until 5 minutes
  before it expires. Never shared between agents. A warm call costs one
  GitHub call.
- ghapp's audit log: every token made, with its hash, the agent and the
  repo, and every tool call with its outcome.
- A footer naming the agent on pull requests, issues and comments, when
  the agent is known. `GHAPP_MCP_AGENT_FOOTER=false` turns it off.
- ghapp's git endpoint. It takes a Straza grant with the holder's proof
  and nothing else, allows exactly the granted branch updates, streams the
  push to GitHub, and logs each git request with Straza's decision id and
  the token's hash.
- Who may call: an OIDC token from a listed client, the gateway secret,
  and the listing token for `repo_list` only. `--dev` serves stdio, or
  loopback without caller authentication, for development and tests.
- A demo: `./demo/run.sh` runs the whole flow offline against stand-ins for
  GitHub, Straza and its holder. See [demo/README.md](../demo/README.md).

Not built:

- `where` in Straza, covering repos by an attribute such as `team`.
- Pushes that change `.github/workflows`. They need GitHub's `workflows`
  permission, which ghapp never asks for, so GitHub refuses them.
- `gh`-backed tools.

## The diagrams

The SVGs are built by [diagrams/tools/build.py](diagrams/tools/build.py).
Edit that file, run it, then check the result with
`python3 docs/diagrams/tools/render.py OUT_DIR`, which writes PNGs of
several points in each loop, light and dark, at 960 and 400 px wide.
