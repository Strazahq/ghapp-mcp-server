# Demo

```
./demo/run.sh
```

It runs the whole flow on this machine, offline, in about ten seconds, and
prints a transcript of ten steps: what the agent sees, the listing, a pull
request on a covered repo and one refused at Straza, then git: a clone and
a push with plain git through Straza's holder, git straight to ghapp past
the holder, a refused tag, a push to a repo no role covers, and the audit
trail of one push. The transcript also goes to `$DEMO_OUT`, by default a
new temp file. It needs Go, git and openssl.

**Real:** `ghapp-mcp-server`, built from this repo for the run, and git
itself.

**Stand-ins, not the real thing:**

- `fakegithub` stands in for GitHub: the App endpoints with a JWT checked
  against the demo App's public key, installation tokens held to their repo
  ids and permissions, the REST calls the tools make, and git over HTTP on
  local bare repos. Its audit log has GitHub's `hashed_token`.
- `fakestraza` stands in for Straza: it runs as `fakestraza mcp ghapp`,
  reads the agent's roles from `roles.json`, lists repos with `repo_list`,
  and forwards a call to ghapp with the gateway secret only when a role
  covers the repo. As `fakestraza grants ghapp` it also issues git grants,
  checked against the same roles, and publishes its signing key.
- `fakeholder` stands in for Straza's holder on the agent's machine: git
  reaches it through one `insteadOf` setting, and it asks for a grant for
  each git request and sends the request on to ghapp with the grant and a
  DPoP proof signed by its own key.

No Keycloak: the stand-in calls ghapp with the gateway secret, so ghapp
cannot name the agent, and adds no footer. `DEMO_KEEP=1` keeps the work
directory, with every log.
