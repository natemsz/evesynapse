/* ---------------------------------------------------------------------------
 * EveSynapse shared-cache seed.
 *
 * A Cloudflare Worker that seeds peer discovery for the EveSynapse
 * instance mesh. It speaks the same wire protocol as the instances
 * themselves (see internal/app/swarm_api.go), so instances need no
 * new discovery code path — a seed URL is dialed exactly like a
 * peer address:
 *
 *   GET  /swarm/members        sealed member list w/ worker signature
 *   POST /swarm/pull           always {"entries": []} (holds no data)
 *   POST /swarm/seed/register  heartbeat: {entry} self-signed announce
 *
 * The worker holds no Eve data and cannot read any: it is a
 * bulletin board for self-signed peer records. Instances verify
 * every record's signature themselves (trust on first use, same
 * as mesh gossip) and spot-check pulled data against ESI before
 * serving it, so a compromised seed can at worst suggest peers
 * that fail verification.
 *
 * Darkness: every route needs the shared bearer secret.
 * Requests without one get 404 (crawlers, scanners); a wrong one
 * gets 403. There is no index, no landing page, no robots file.
 *
 * Storage is one KV namespace (SEED_PEERS):
 *   "seed-identity" -> {id, publicJwk, privateJwk} (minted once)
 *   "peer:<instance_id>" -> {base_url, public_key, last_seen}
 *     with a 48h TTL, refreshed by each heartbeat.
 *
 * Secrets (wrangler secret put):
 *   SHARED_CACHE_SECRET  the same secret the instances share
 *
 * No npm dependencies: WebCrypto only (HKDF-SHA-256, AES-256-GCM,
 * Ed25519), byte-compatible with the Go implementation.
 * ------------------------------------------------------------------------- */

const SEAL_PREFIX = "swm:v1:";
const SEAL_SALT = "evesynapse/swarm-members/v1";
const SEAL_INFO = "swarm member list";
const PEER_TTL_SECONDS = 48 * 3600;
const MAX_STORED_PEERS = 1000;
const MAX_SERVED_PEERS = 500;
const MAX_BODY_BYTES = 1 << 20;
const SEED_INSTANCE_ID = "seed";

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, "") || "/";
    if (path !== "/swarm/members" && path !== "/swarm/pull" && path !== "/swarm/seed/register") {
      return notFound();
    }
    const secret = (env.SHARED_CACHE_SECRET || "").trim();
    if (!secret) {
      // Misconfigured worker: dark, like everything else here.
      return notFound();
    }
    const auth = authResult(request, secret);
    if (auth === "missing") return notFound();
    if (auth === "wrong") return forbidden();
    try {
      if (request.method === "GET" && path === "/swarm/members") {
        return await handleMembers(env, secret, url.origin);
      }
      if (request.method === "POST" && path === "/swarm/pull") {
        return json({ entries: [] });
      }
      if (request.method === "POST" && path === "/swarm/seed/register") {
        return await handleRegister(env, request);
      }
      return notFound();
    } catch (err) {
      // Never leak internals; a failing seed just looks away.
      return notFound();
    }
  },
};

/* --- auth -------------------------------------------------------- */

function notFound() {
  return new Response("not found", { status: 404 });
}

function forbidden() {
  return new Response("forbidden", { status: 403 });
}

function json(obj) {
  return new Response(JSON.stringify(obj), {
    status: 200,
    headers: { "content-type": "application/json" },
  });
}

// "missing" (dark 404) when no bearer travels at all, "wrong"
// (403) when one does but mismatches, "ok" otherwise. Hashes
// are compared, in constant time, like the Go side.
async function authResult(request, secret) {
  const header = request.headers.get("authorization") || "";
  if (!header) return "missing";
  const prefix = "Bearer ";
  if (!header.startsWith(prefix) || header.length <= prefix.length) return "missing";
  const presented = header.slice(prefix.length).trim();
  if (!presented) return "missing";
  const want = await crypto.subtle.digest("SHA-256", textEncoder.encode(secret));
  const got = await crypto.subtle.digest("SHA-256", textEncoder.encode(presented));
  const a = new Uint8Array(want);
  const b = new Uint8Array(got);
  if (a.length !== b.length) return "wrong";
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0 ? "ok" : "wrong";
}

/* --- members ----------------------------------------------------- */

async function handleMembers(env, secret, origin) {
  const identity = await seedIdentity(env);
  const peers = await listPeers(env);
  const members = [
    { instance_id: SEED_INSTANCE_ID, base_url: origin, public_key: identity.publicB64 },
    ...peers.map((p) => ({ instance_id: p.id, base_url: p.base_url, public_key: p.public_key })),
  ];
  const sealed = await sealMembers(JSON.stringify(members), secret);
  const entry = await signEntry(
    identity,
    "swarm_members",
    SEED_INSTANCE_ID,
    sealed,
    Math.floor(Date.now() / 1000)
  );
  return json({ entry });
}

async function listPeers(env) {
  const listed = await env.SEED_PEERS.list({ limit: MAX_STORED_PEERS });
  const out = [];
  for (const key of listed.keys) {
    if (!key.name.startsWith("peer:")) continue;
    const rec = await env.SEED_PEERS.get(key.name, { type: "json" });
    if (!rec || typeof rec.base_url !== "string" || typeof rec.public_key !== "string") continue;
    out.push({ id: key.name.slice("peer:".length), base_url: rec.base_url, public_key: rec.public_key, last_seen: rec.last_seen || 0 });
  }
  out.sort((a, b) => b.last_seen - a.last_seen);
  return out.slice(0, MAX_SERVED_PEERS);
}

/* --- register ---------------------------------------------------- */

async function handleRegister(env, request) {
  const body = await readCappedJSON(request);
  if (!body || body.entry === undefined) {
    return new Response("bad request", { status: 400 });
  }
  const entry = body.entry;
  if (!isAnnounceEntry(entry)) {
    return new Response("bad request", { status: 400 });
  }
  let detail;
  try {
    detail = JSON.parse(entry.payload);
  } catch {
    return new Response("bad request", { status: 400 });
  }
  if (!detail || typeof detail.base_url !== "string" || typeof detail.public_key !== "string") {
    return new Response("bad request", { status: 400 });
  }
  if (!isHttpsURL(detail.base_url)) {
    return new Response("bad request", { status: 400 });
  }
  // Self-signed: the claimed key must verify the envelope.
  // Same trust-on-first-use as mesh gossip.
  if (!(await verifyEntry(entry, detail.public_key))) {
    return new Response("bad request", { status: 400 });
  }
  const stored = await env.SEED_PEERS.list({ limit: MAX_STORED_PEERS });
  const peerKey = "peer:" + entry.signer;
  const exists = stored.keys.some((k) => k.name === peerKey);
  if (!exists && stored.keys.filter((k) => k.name.startsWith("peer:")).length >= MAX_STORED_PEERS) {
    return new Response("busy", { status: 429 });
  }
  await env.SEED_PEERS.put(
    peerKey,
    JSON.stringify({ base_url: detail.base_url, public_key: detail.public_key, last_seen: Math.floor(Date.now() / 1000) }),
    { expirationTtl: PEER_TTL_SECONDS }
  );
  return json({ ok: true });
}

function isAnnounceEntry(entry) {
  if (!entry || typeof entry !== "object") return false;
  const keys = Object.keys(entry).sort();
  const want = ["fetched_at", "key", "kind", "payload", "signature", "signer"];
  if (keys.length !== want.length || !keys.every((k, i) => k === want[i])) return false;
  if (entry.kind !== "announce") return false;
  if (typeof entry.key !== "string" || entry.key === "") return false;
  if (entry.key !== entry.signer) return false;
  if (typeof entry.payload !== "string" || entry.payload === "") return false;
  if (typeof entry.signature !== "string" || entry.signature === "") return false;
  if (typeof entry.fetched_at !== "number" || !Number.isInteger(entry.fetched_at)) return false;
  if (entry.payload.length > 256 * 1024) return false;
  return true;
}

function isHttpsURL(raw) {
  try {
    const u = new URL(raw);
    return u.protocol === "https:";
  } catch {
    return false;
  }
}

async function readCappedJSON(request) {
  const reader = request.body.getReader();
  let received = 0;
  const chunks = [];
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    received += value.byteLength;
    if (received > MAX_BODY_BYTES) return null;
    chunks.push(value);
  }
  const raw = concatBytes(chunks);
  try {
    return JSON.parse(textDecoder.decode(raw));
  } catch {
    return null;
  }
}

function concatBytes(chunks) {
  let total = 0;
  for (const c of chunks) total += c.byteLength;
  const out = new Uint8Array(total);
  let at = 0;
  for (const c of chunks) {
    out.set(c, at);
    at += c.byteLength;
  }
  return out;
}

/* --- identity ---------------------------------------------------- */

async function seedIdentity(env) {
  const stored = await env.SEED_PEERS.get("seed-identity", { type: "json" });
  if (stored && stored.publicJwk && stored.privateJwk) {
    const [publicKey, privateKey] = await Promise.all([
      crypto.subtle.importKey("jwk", stored.publicJwk, { name: "Ed25519" }, true, ["verify"]),
      crypto.subtle.importKey("jwk", stored.privateJwk, { name: "Ed25519" }, true, ["sign"]),
    ]);
    return { ...stored, publicKey, privateKey };
  }
  const pair = await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
  const [publicJwk, privateJwk] = await Promise.all([
    crypto.subtle.exportKey("jwk", pair.publicKey),
    crypto.subtle.exportKey("jwk", pair.privateKey),
  ]);
  const raw = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  const rec = { id: SEED_INSTANCE_ID, publicJwk, privateJwk, publicB64: b64encode(raw) };
  await env.SEED_PEERS.put("seed-identity", JSON.stringify({ id: rec.id, publicJwk, privateJwk, publicB64: rec.publicB64 }));
  return { ...rec, publicKey: pair.publicKey, privateKey: pair.privateKey };
}

/* --- wire format (Go-compatible) --------------------------------- */

// Canonical envelope bytes: struct field order, compact, with
// Go's string escaping (<, >, & as \u00XX). Signer and verifier
// must hash identical bytes without a canonicalizer.
function envelopeBytes(v, kind, key, payload, fetchedAt, signer) {
  return textEncoder.encode(
    '{"v":' + v +
      ',"kind":' + goString(kind) +
      ',"key":' + goString(key) +
      ',"payload":' + goString(payload) +
      ',"fetched_at":' + fetchedAt +
      ',"signer":' + goString(signer) +
      "}"
  );
}

function goString(s) {
  let out = '"';
  for (const ch of s) {
    const code = ch.codePointAt(0);
    if (ch === '"') out += '\\"';
    else if (ch === "\\") out += "\\\\";
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    else if (ch === "<") out += "\\u003c";
    else if (ch === ">") out += "\\u003e";
    else if (ch === "&") out += "\\u0026";
    else if (code < 0x20) out += "\\u" + code.toString(16).padStart(4, "0");
    else out += ch;
  }
  return out + '"';
}

async function signEntry(identity, kind, key, payload, fetchedAt) {
  const doc = envelopeBytes(1, kind, key, payload, fetchedAt, identity.id);
  const sig = new Uint8Array(await crypto.subtle.sign({ name: "Ed25519" }, identity.privateKey, doc));
  return { kind, key, payload, fetched_at: fetchedAt, signer: identity.id, signature: b64encode(sig) };
}

async function verifyEntry(entry, publicB64) {
  try {
    const raw = b64decode(publicB64);
    if (raw.byteLength !== 32) return false;
    const key = await crypto.subtle.importKey("raw", raw, { name: "Ed25519" }, false, ["verify"]);
    const sig = b64decode(entry.signature);
    if (sig.byteLength !== 64) return false;
    const doc = envelopeBytes(1, entry.kind, entry.key, entry.payload, entry.fetched_at, entry.signer);
    return await crypto.subtle.verify({ name: "Ed25519" }, key, sig, doc);
  } catch {
    return false;
  }
}

// Seal mirrors sealSwarmMembers: HKDF-SHA-256 over the secret
// (salt + info pinned), AES-256-GCM with a random 12-byte
// nonce prepended, base64, "swm:v1:" prefix.
async function sealMembers(raw, secret) {
  const keyMaterial = await crypto.subtle.importKey("raw", textEncoder.encode(secret), { name: "HKDF" }, false, ["deriveBits"]);
  const keyBytes = new Uint8Array(
    await crypto.subtle.deriveBits(
      { name: "HKDF", hash: "SHA-256", salt: textEncoder.encode(SEAL_SALT), info: textEncoder.encode(SEAL_INFO) },
      keyMaterial,
      256
    )
  );
  const key = await crypto.subtle.importKey("raw", keyBytes, { name: "AES-GCM" }, false, ["encrypt"]);
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, key, textEncoder.encode(raw)));
  const out = new Uint8Array(nonce.byteLength + ct.byteLength);
  out.set(nonce, 0);
  out.set(ct, nonce.byteLength);
  return SEAL_PREFIX + b64encode(out);
}

function b64encode(bytes) {
  let s = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.byteLength; i += chunk) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
  }
  // Standard alphabet: btoa already emits +/=, no translation.
  return btoa(s);
}

function b64decode(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
