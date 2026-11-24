-- Schema step 181: home widget settings share one shape.
--
-- Every widget's settings are one JSON object of option values, keyed
-- by option. The Market widget stored its own shape (scope_type,
-- character_id, tag, merge); this rewrites those rows into its two
-- options: scope ("all", "char:<id>", "tag:<name>") and show (how
-- several characters read: "both", "combined", "per-character").
-- Values the app no longer accepts fall back to the defaults when read.
UPDATE widget_configs
SET config = json_build_object(
        'scope', CASE config::json ->> 'scope_type'
                     WHEN 'character' THEN 'char:' || COALESCE(config::json ->> 'character_id', '')
                     WHEN 'tag' THEN 'tag:' || COALESCE(config::json ->> 'tag', '')
                     ELSE 'all'
                 END,
        'show', COALESCE(NULLIF(config::json ->> 'merge', ''), 'both')
    )::text
WHERE widget_id = 'market' AND config LIKE '%"scope_type"%';
