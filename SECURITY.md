# Security policy

## Supported versions

Security fixes go into the newest release. Versions before 1.0.0 get no
backports, so upgrade to the newest 0.x release to get a fix.

## Reporting a vulnerability

Report it privately through GitHub: open the Security tab of this
repository and choose Report a vulnerability. Do not open a public issue
for it.

Say what an attacker can do, which version and configuration it needs, and
the steps to reproduce it. A test App, a made-up repo and made-up secrets
are enough. Never send a real GitHub App private key, an installation
token, a gateway secret, a listing token or a Straza grant.

These count as vulnerabilities in this server:

- A tool call at `/mcp` that passes without the gateway secret or an
  accepted OIDC token, outside `--dev`.
- A listing token that reaches any tool other than `repo_list`.
- A call that reaches a repo other than the one it names, or a repo id
  other than the one Straza approved, for example through a rename, a
  redirect or a repo deleted and created again.
- A GitHub token with more repos or permissions than its tool needs, or
  one shared between two callers.
- The App's private key or a GitHub token in a tool result, an error, a
  log line or anything else that leaves the server.
- A request at `/git` that passes without a valid Straza grant and the
  holder's proof, or a grant or proof that opens more than its one request
  on one instance.
- A push that reaches GitHub with ref updates other than the granted ones,
  or with a delete, a tag, a ref outside `refs/heads/` or a push option, or
  any push in read-only mode.
- A grant, a proof, a secret or a token in a log line.

These are not:

- What Straza allows. Straza decides which agent may call which tool on
  which repo, and whether a person approves.
- What GitHub allows a push to do, such as a force push. GitHub's branch
  rules decide that.
- A grant used once on each of several instances. ghapp keeps spent
  grants per instance, as README.md says.
