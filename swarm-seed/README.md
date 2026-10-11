# EveSynapse shared-cache seed

A Cloudflare Worker that seeds peer discovery for the EveSynapse
instance mesh. Instances already dial seeds exactly like peers
(`SHARED_CACHE_SEEDS`, gossip source `seed`), so the worker
speaks the identical wire protocol — sealed member lists and
Ed25519-signed entries, byte-compatible with `swarm_api.go`:

- `GET /swarm/members` → `{"entry": ...}`: the worker plus every
  registered peer, sealed under the shared secret, signed by the
  worker's own KV-persisted identity (pinned on first use).
- `POST /swarm/pull` → `{"entries": []}`: the seed holds no Eve
  data and serves none.
- `POST /swarm/seed/register` → `{"ok": true}`: heartbeat carrying
  the instance's self-signed announce entry (`kind: "announce"`,
  `key`/`signer` = instance id, payload = its base URL and public
  key). The worker verifies the signature against the claimed key
  (same trust-on-first-use as mesh gossip), requires an `https:`
  base URL, and stores the record with a 48h TTL. Instances
  re-announce every gossip round (12–24h), so dead peers age out
  on their own. At most 1000 peers stored, 500 served, newest
  heartbeats first.

Instances with no public address configured do not announce:
a URL nobody can dial would only pollute the directory.

## Darkness (not crawlable, not indexable)

- No index, no landing page, no robots file — unknown paths 404.
- Known paths without a bearer secret 404 (scanners, crawlers).
- A present-but-wrong bearer 403 (operator-typo signal only).
- Failures answer 404 with no reason body; nothing identifies
  the worker as EveSynapse-related.
- Knowing the worker URL grants nothing without the secret, and
  sealed responses stay opaque to non-secret-holders.

A compromised seed can at worst suggest peers: instances verify
every record signature themselves and spot-check pulled data
against ESI before serving it.

## Deploy

1. `wrangler kv:namespace create SEED_PEERS`, put the id in
   `wrangler.toml`.
2. `wrangler secret put SHARED_CACHE_SECRET` — the exact value
   from the instances' `.env` (32+ hex chars).
3. `wrangler deploy`, then set that URL as the instances'
   `SHARED_CACHE_SEEDS`.

No npm dependencies; WebCrypto only. The byte layout it must
match (envelope key order, Go string escaping, `swm:v1:`
seal envelope, HKDF salt/info) is pinned by
`TestSeedWorkerInterop` in `internal/app/swarm_seed_test.go`,
which opens a worker-format sealed blob produced by an
independent implementation — if either side drifts, that test
goes red.
