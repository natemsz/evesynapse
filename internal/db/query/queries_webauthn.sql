-- WebAuthn two-factor queries (auth_webauthn.go).

-- name: CreateWebAuthnCredential :one
INSERT INTO webauthn_credentials (user_id, credential_id, public_key, sign_count, transports, attestation_format, name)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListWebAuthnCredentialsByUser :many
SELECT * FROM webauthn_credentials WHERE user_id = $1 ORDER BY created_at, id;

-- name: GetWebAuthnCredential :one
SELECT * FROM webauthn_credentials WHERE credential_id = $1;

-- name: UpdateWebAuthnCredentialSignCount :exec
UPDATE webauthn_credentials SET sign_count = $2, last_used_at = now() WHERE credential_id = $1;

-- name: DeleteWebAuthnCredential :exec
DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2;

-- name: CountWebAuthnCredentialsByUser :one
SELECT count(*) FROM webauthn_credentials WHERE user_id = $1;

-- name: CreateWebAuthnRecoveryCode :one
INSERT INTO webauthn_recovery_codes (user_id, code_hash) VALUES ($1, $2) RETURNING *;

-- name: ListUnusedWebAuthnRecoveryCodesByUser :many
SELECT * FROM webauthn_recovery_codes WHERE user_id = $1 AND used_at IS NULL ORDER BY id;

-- name: BurnWebAuthnRecoveryCode :exec
UPDATE webauthn_recovery_codes SET used_at = now() WHERE id = $1 AND used_at IS NULL;

-- name: DeleteWebAuthnRecoveryCodesByUser :exec
DELETE FROM webauthn_recovery_codes WHERE user_id = $1;

-- name: CountUnusedWebAuthnRecoveryCodesByUser :one
SELECT count(*) FROM webauthn_recovery_codes WHERE user_id = $1 AND used_at IS NULL;

-- name: InsertWebAuthnEvent :exec
INSERT INTO webauthn_events (user_id, kind) VALUES ($1, $2);

-- name: ListWebAuthnEventsByUser :many
SELECT * FROM webauthn_events WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT 50;

-- name: PruneWebAuthnEvents :execrows
DELETE FROM webauthn_events WHERE created_at < $1::timestamptz;
