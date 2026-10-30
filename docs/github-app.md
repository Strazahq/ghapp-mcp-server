# Create the GitHub App

ghapp works as one GitHub App. You create the App once, install it on the
repos it may reach, and give ghapp the App id and the private key.

## 1. Register the App

Create it under the organization that owns the repos:
`https://github.com/organizations/<org>/settings/apps/new`.

| Field | Value |
|---|---|
| GitHub App name | Anything, such as `ghapp-<org>`. GitHub shows it as `<name>[bot]` on every pull request and comment |
| Homepage URL | Any URL, such as your org's site |
| Webhook | Untick **Active**. ghapp uses no webhooks |
| Where can this GitHub App be installed? | **Only on this account** |

A private App installs only on the account that owns it. So register it
under the organization, not under your personal account.

## 2. Permissions

| Permission | Level | Needed by |
|---|---|---|
| Repository: **Metadata** | Read (always on) | every tool: the name and id check, `repo_get`, `repo_list` |
| Repository: **Contents** | Read and write | read: `branch_list`, `file_get`, `search_code`, `git_fetch`, and `pr_create`, because GitHub refuses a pull request on a private repo whose token can't read the branches. Write: `pr_merge`, `git_push` |
| Repository: **Pull requests** | Read and write | read: `pr_list`, `pr_get`, `search_issues`. Write: `pr_create`, `pr_comment`, `pr_merge` |
| Repository: **Issues** | Read and write | read: `issue_list`, `issue_get`, `search_issues`. Write: `issue_create`, `issue_comment` |
| Organization: **Custom properties** | Read | `repo_list`, for the attributes in `GHAPP_MCP_ATTRIBUTES` such as `team` |

Two permissions are called **Custom properties**. ghapp needs the one under
**Organization permissions**. The one under Repository permissions
(`repository_custom_properties`) doesn't read the organization's property
list, so `repo_list` would report no attributes.

Leave everything else at **No access**. For a read-only deployment
(`GHAPP_MCP_READ_ONLY=true`), give Contents, Pull requests and Issues read
only.

Each call still gets a token for one repo and only the permissions its tool
needs. The App's permissions are only the ceiling.

Not granted on purpose: **Workflows**. A push that changes a file under
`.github/workflows` needs it, so GitHub refuses such a push, and git
prints: "refusing to allow a GitHub App to create or update workflow
... without `workflows` permission". Who may change CI is a decision for a
later release.

## 3. Get the id and the key

1. On the App's page, note the **App ID** (a number).
2. Under **Private keys**, click **Generate a private key**. GitHub
   downloads a `.pem` file. Keep it like a password. ghapp reads it from
   `GHAPP_PRIVATE_KEY_FILE` and never sends it anywhere.

## 4. Install the App

On the App's page, open **Install App**, choose the organization, and pick
**All repositories** or **Only select repositories**. ghapp reaches only
what this install covers.

## 5. Custom properties (optional)

`repo_list` reports GitHub custom properties beside each repo, such as
`team`, so that Straza can show them and, once Straza covers repos by
attribute, match a role to them.

1. In the organization's settings, open **Custom properties** and add a
   property such as `team`.
2. Set it on each repo.
3. Start ghapp with `GHAPP_MCP_ATTRIBUTES=team`.

Who may set the property matters. If repo admins may set it, any repo admin
could move their repo into a role that covers repos by that property.

## 6. Run ghapp

```
GHAPP_APP_ID=<App ID> GHAPP_PRIVATE_KEY_FILE=/path/to/key.pem \
GHAPP_MCP_GATEWAY_SECRET=<the secret Straza sends> \
  ghapp-mcp-server --http 0.0.0.0:8080
```

The README lists every setting, and how Straza reaches ghapp.

## Seen against a real App

A private App on a test organization, with three private repos.

| What | Result |
|---|---|
| `repo_list` | The repos with GitHub's ids and the `team` property |
| Reads, issues, pull requests, comments, a merge | Work. `pr_merge` works with Contents write and Pull requests write |
| `pr_create` with Pull requests write only | GitHub refuses: 422 "not all refs are readable". So `pr_create` also asks for Contents read |
| `GET /repositories/{id}`, the lookup by the id Straza approved | Works, though GitHub's docs don't list the route |
| A wrong approved id, a name in the wrong case, a missing repo | Refused with `repo-id-mismatch`, `repo-not-canonical`, `not-found` |
| git clone, fetch and push through Straza's holder and ghapp's git endpoint | Work |
| A push that changes `.github/workflows` | GitHub refuses it, as above |
| Issue search on a private repo | Works |
| Code search on new repos | No hits until GitHub has indexed them |

## Sources

- https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps
- https://docs.github.com/en/rest/orgs/custom-properties
- https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app
- https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app
- https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/making-a-github-app-public-or-private
