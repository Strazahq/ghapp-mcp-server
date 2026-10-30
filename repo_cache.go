package main

import "time"

// The lookup caches let a warm tool call spend one GitHub call, the tool's
// own. Each holds only answers GitHub gave and access then accepted, never a
// refusal or an error, and each entry only ever lets a call go ahead: an
// answer from a cache that would refuse is read fresh from GitHub instead.
//
// Why each is safe:
//
//   - Repo name to installation, 10 minutes. The installation only says
//     where to mint. The token is still minted for the one repo id and the
//     tool's permissions, so a stale installation reaches nothing more:
//     GitHub refuses the mint with 422, or the token's calls with 404, and
//     the entries are dropped and the call retried from cold.
//   - Repo id to canonical name, 30 seconds, for calls with Straza's id. A
//     hit lets the id path skip RepositoryByID. The token is minted for that
//     id, so a stale name can never write to another repo. A rename shows
//     as a redirect on the old name, which is refused as repo-renamed. A
//     repo deleted and created again under the name has a new id, which the
//     old token does not reach: GitHub answers 404, and the retry from cold
//     refuses with repo-id-mismatch. One limit: any GitHub token reads a
//     public repo, so a read in those 30 seconds of a public repo created
//     under the old name answers with that public repo's data, as it would
//     to any GitHub client.
//   - Canonical name to repo id, 60 seconds, for calls without an id, which
//     come through a role on every repo. The token is minted for the cached
//     id, with the same reasoning: a renamed repo redirects, a recreated one
//     answers 404 to the old token and the retry resolves the new id.
//
// The keys are exact strings: a name that differs in case from GitHub's is
// never stored as a name, and is refused fresh each time.
const (
	installTTL     = 10 * time.Minute
	canonicalTTL   = 30 * time.Second
	nameTTL        = 60 * time.Second
	maxCachedRepos = 20_000
)

// repoCache holds the three lookups. It is safe for concurrent use.
type repoCache struct {
	installs  *lru[string, int64] // repo name as called, to its installation
	canonical *lru[int64, string] // repo id, to its canonical full name
	names     *lru[string, int64] // canonical full name, to its repo id
}

func newRepoCache() *repoCache {
	return &repoCache{
		installs:  newLRU[string, int64](maxCachedRepos),
		canonical: newLRU[int64, string](maxCachedRepos),
		names:     newLRU[string, int64](maxCachedRepos),
	}
}

// forget drops every entry for the repo name and the repo id, so the next
// access reads both fresh.
func (c *repoCache) forget(name string, id int64) {
	if name != "" {
		c.installs.remove(name)
		c.names.remove(name)
	}
	if id > 0 {
		c.canonical.remove(id)
	}
}
