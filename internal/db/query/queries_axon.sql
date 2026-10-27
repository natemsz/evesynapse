-- name: InsertIntelSubmitLabel :exec
INSERT INTO intel_submit_labels (channel_id, user_id, raw, detected_kind, detected_system_id,
    override_kind, override_system_id, posted, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
