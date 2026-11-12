-- 必须先读同目录上线手册。默认演练；实际提交只改最后一行。快照永久保留。
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';
SET LOCAL search_path = public, pg_catalog;

DO $tier_rollback$
DECLARE
    -- 参数：没有可直接恢复的快照分组时，回到该新档对应的最低旧 key。
    reverse_mapping jsonb := '{"Start":"Lv1","Build":"Lv2","Scale":"Lv4","Enterprise":"Lv6"}';
    mapping jsonb;
    old_keys text[];
    new_keys text[];
    snapshot_name text;
    migration_status text;
    item record;
    bad_ids text;
    restored bigint;
    expected_restored bigint;
    reverse_count bigint;
    changed_attributes bigint;
    total_before bigint;
    expected_counts jsonb;
    current_ratio jsonb;
    restored_ratio jsonb;
    original_ratio_text text;
    restored_ratio_text text;
BEGIN
    IF pg_is_in_recovery() THEN
        RAISE EXCEPTION 'Connected to a read-only standby; reconnect to the primary before running this script';
    END IF;
    FOREACH snapshot_name IN ARRAY ARRAY['state','users','channels','abilities','group_configs','options','abilities_after','group_configs_after','options_after'] LOOP
        IF to_regclass('tier_rename_20261112_' || snapshot_name) IS NULL THEN
            RAISE EXCEPTION 'Required snapshot is missing: tier_rename_20261112_%', snapshot_name;
        END IF;
    END LOOP;
    LOCK TABLE users, channels, abilities, group_configs, options, tier_rename_20261112_state IN SHARE ROW EXCLUSIVE MODE;
    IF (SELECT count(*) FROM tier_rename_20261112_state) <> 1 THEN
        RAISE EXCEPTION 'Invalid migration state snapshot';
    END IF;
    SELECT key_mapping, status INTO mapping, migration_status FROM tier_rename_20261112_state;
    IF migration_status = 'rolled_back' THEN
        RAISE NOTICE 'Already rolled back: no changes made';
        RETURN;
    END IF;
    IF migration_status <> 'applied' THEN
        RAISE EXCEPTION 'Unexpected migration status: %', migration_status;
    END IF;
    SELECT array_agg(key), array_agg(DISTINCT value ORDER BY value) INTO old_keys, new_keys FROM jsonb_each_text(mapping);
    IF jsonb_typeof(reverse_mapping) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'reverse_mapping must be an object';
    END IF;
    IF (SELECT array_agg(key ORDER BY key) FROM jsonb_each_text(reverse_mapping)) IS DISTINCT FROM new_keys
       OR EXISTS (SELECT 1 FROM jsonb_each_text(reverse_mapping) r WHERE mapping->>r.value IS DISTINCT FROM r.key) THEN
        RAISE EXCEPTION 'Invalid reverse_mapping: each target must be an old key mapped to that new tier';
    END IF;

    SELECT string_agg(id::text, ', ' ORDER BY id) INTO bad_ids FROM (
        SELECT s.id FROM tier_rename_20261112_channels s LEFT JOIN channels c ON c.id = s.id
        WHERE c."group" IS DISTINCT FROM s.mapped_group
        UNION
        SELECT c.id FROM channels c CROSS JOIN LATERAL unnest(string_to_array(c."group", ',')) k
        WHERE btrim(k) = ANY(new_keys) AND NOT EXISTS (SELECT 1 FROM tier_rename_20261112_channels s WHERE s.id = c.id)
        ORDER BY id LIMIT 20
    ) conflicts;
    IF bad_ids IS NOT NULL THEN
        RAISE EXCEPTION 'Channels changed after migration; channel ids (first 20): %', bad_ids;
    END IF;
    -- 只检查结构；启停、优先级属于正常运行变化，恢复时使用当前值。
    WITH differences AS (
        (SELECT "group", model, channel_id FROM abilities WHERE "group" = ANY(new_keys || old_keys)
         EXCEPT SELECT "group", model, channel_id FROM tier_rename_20261112_abilities_after)
        UNION
        (SELECT "group", model, channel_id FROM tier_rename_20261112_abilities_after
         EXCEPT SELECT "group", model, channel_id FROM abilities WHERE "group" = ANY(new_keys || old_keys))
    )
    SELECT string_agg(channel_id::text, ', ' ORDER BY channel_id) INTO bad_ids
    FROM (SELECT DISTINCT channel_id FROM differences ORDER BY channel_id LIMIT 20) conflicts;
    IF bad_ids IS NOT NULL THEN
        RAISE EXCEPTION 'Ability structure changed after migration; channel ids (first 20): %', bad_ids;
    END IF;
    IF EXISTS (SELECT 1 FROM unnest(new_keys) k WHERE NOT EXISTS (SELECT 1 FROM group_configs WHERE group_key = k))
       OR EXISTS (SELECT 1 FROM group_configs WHERE group_key = ANY(old_keys))
       OR EXISTS (SELECT 1 FROM group_configs g JOIN tier_rename_20261112_group_configs s ON g.id = s.id WHERE NOT (g.group_key = ANY(new_keys))) THEN
        RAISE EXCEPTION 'Tier configuration conflict: missing new tier, existing old key or occupied snapshot id';
    END IF;
    SELECT value::jsonb INTO current_ratio FROM options WHERE key = 'GroupRatio';
    IF FOUND AND (current_ratio IS NULL OR jsonb_typeof(current_ratio) <> 'object') THEN
        RAISE EXCEPTION 'Current GroupRatio must be a JSON object';
    END IF;
    current_ratio := coalesce(current_ratio, '{}'::jsonb);
    IF current_ratio ?| old_keys THEN
        RAISE EXCEPTION 'GroupRatio contains old keys; reconcile them before rollback';
    END IF;
    SELECT value INTO original_ratio_text FROM tier_rename_20261112_options;
    SELECT (current_ratio - new_keys - old_keys) || coalesce(jsonb_object_agg(r.key, r.value), '{}'::jsonb)
    INTO restored_ratio FROM jsonb_each(coalesce(original_ratio_text::jsonb, '{}'::jsonb)) r WHERE r.key = ANY(old_keys);
    -- 内容相同时保留原 JSON 文本，既能逐项复原，也不会覆盖当前自定义倍率。
    restored_ratio_text := CASE WHEN restored_ratio = original_ratio_text::jsonb THEN original_ratio_text ELSE restored_ratio::text END;

    RAISE NOTICE 'Current new tier configurations before rollback: %',
        (SELECT jsonb_agg(to_jsonb(g) ORDER BY sort_order, id) FROM group_configs g WHERE group_key = ANY(new_keys));
    RAISE NOTICE 'Current GroupRatio before rollback: %', (SELECT value FROM options WHERE key = 'GroupRatio');
    SELECT count(*) INTO reverse_count FROM users u LEFT JOIN tier_rename_20261112_users s ON s.id = u.id
    WHERE u."group" = ANY(new_keys) AND (s.id IS NULL OR u."group" IS DISTINCT FROM s.mapped_group);
    RAISE NOTICE 'Users using reverse mapping: total=%, showing at most 50', reverse_count;
    FOR item IN
        SELECT u.id, u."group" AS current_group, reverse_mapping->>u."group" AS target
        FROM users u LEFT JOIN tier_rename_20261112_users s ON s.id = u.id
        WHERE u."group" = ANY(new_keys) AND (s.id IS NULL OR u."group" IS DISTINCT FROM s.mapped_group)
        ORDER BY u.id LIMIT 50
    LOOP
        RAISE NOTICE 'User reverse mapping: id=%, current=%, target=%', item.id, item.current_group, item.target;
    END LOOP;
    SELECT count(*) INTO total_before FROM users;
    SELECT count(*) INTO expected_restored FROM users WHERE "group" = ANY(new_keys);
    SELECT jsonb_object_agg(k, n) INTO expected_counts FROM (
        SELECT coalesce(CASE WHEN u."group" = s.mapped_group THEN s."group"
                             ELSE coalesce(reverse_mapping->>u."group", u."group") END, '<NULL>') k, count(*) n
        FROM users u LEFT JOIN tier_rename_20261112_users s ON s.id = u.id GROUP BY 1
    ) counts;
    SELECT count(*) INTO changed_attributes FROM tier_rename_20261112_abilities s
    JOIN abilities a ON a."group" = mapping->>s."group" AND a.model = s.model AND a.channel_id = s.channel_id
    WHERE a.enabled IS DISTINCT FROM s.enabled OR a.priority IS DISTINCT FROM s.priority;
    RAISE NOTICE 'Restored ability rows with attributes different from original snapshot: %', changed_attributes;

    UPDATE users u SET "group" = changes.target FROM (
        SELECT u.id, CASE WHEN u."group" = s.mapped_group THEN s."group" ELSE reverse_mapping->>u."group" END AS target
        FROM users u LEFT JOIN tier_rename_20261112_users s ON s.id = u.id WHERE u."group" = ANY(new_keys)
    ) changes WHERE u.id = changes.id;
    GET DIAGNOSTICS restored = ROW_COUNT;
    UPDATE channels c SET "group" = s."group" FROM tier_rename_20261112_channels s WHERE c.id = s.id;
    -- 先插旧行才能使用当前新行的运行状态；校验后删除所有新行（包括继承新增行）。
    INSERT INTO abilities ("group", model, channel_id, enabled, priority)
        SELECT s."group", s.model, s.channel_id, a.enabled, a.priority FROM tier_rename_20261112_abilities s
        JOIN abilities a ON a."group" = mapping->>s."group" AND a.model = s.model AND a.channel_id = s.channel_id;
    IF EXISTS (
        (SELECT s."group", s.model, s.channel_id, a.enabled, a.priority FROM tier_rename_20261112_abilities s
         JOIN abilities a ON a."group" = mapping->>s."group" AND a.model = s.model AND a.channel_id = s.channel_id
         EXCEPT SELECT "group", model, channel_id, enabled, priority FROM abilities WHERE "group" = ANY(old_keys))
        UNION ALL
        (SELECT "group", model, channel_id, enabled, priority FROM abilities WHERE "group" = ANY(old_keys)
         EXCEPT SELECT s."group", s.model, s.channel_id, a.enabled, a.priority FROM tier_rename_20261112_abilities s
         JOIN abilities a ON a."group" = mapping->>s."group" AND a.model = s.model AND a.channel_id = s.channel_id)
    ) THEN
        RAISE EXCEPTION 'Rollback assertion failed: ability attributes do not match current mapped rows';
    END IF;
    DELETE FROM abilities WHERE "group" = ANY(new_keys);
    DELETE FROM group_configs WHERE group_key = ANY(new_keys);
    INSERT INTO group_configs SELECT * FROM tier_rename_20261112_group_configs;
    DELETE FROM options WHERE key = 'GroupRatio';
    IF restored_ratio <> '{}'::jsonb OR original_ratio_text IS NOT NULL THEN
        INSERT INTO options (key, value) VALUES ('GroupRatio', restored_ratio_text);
    END IF;

    IF (SELECT count(*) FROM users) <> total_before OR restored <> expected_restored
       OR (SELECT jsonb_object_agg(k, n) FROM (SELECT coalesce("group", '<NULL>') k, count(*) n FROM users GROUP BY 1) counts) IS DISTINCT FROM expected_counts
       OR EXISTS (SELECT 1 FROM users WHERE "group" = ANY(new_keys))
       OR EXISTS (SELECT 1 FROM channels c CROSS JOIN LATERAL unnest(string_to_array(c."group", ',')) k WHERE btrim(k) = ANY(new_keys))
       OR EXISTS (SELECT 1 FROM tier_rename_20261112_channels s LEFT JOIN channels c ON c.id = s.id WHERE c."group" IS DISTINCT FROM s."group")
       OR EXISTS (SELECT 1 FROM abilities WHERE "group" = ANY(new_keys))
       OR EXISTS (
          (SELECT "group", model, channel_id FROM abilities WHERE "group" = ANY(old_keys)
           EXCEPT SELECT "group", model, channel_id FROM tier_rename_20261112_abilities)
          UNION ALL
          (SELECT "group", model, channel_id FROM tier_rename_20261112_abilities
           EXCEPT SELECT "group", model, channel_id FROM abilities WHERE "group" = ANY(old_keys))
       ) OR EXISTS (SELECT 1 FROM group_configs WHERE group_key = ANY(new_keys))
       OR EXISTS (
          (SELECT to_jsonb(g) FROM group_configs g WHERE group_key = ANY(old_keys) EXCEPT SELECT to_jsonb(s) FROM tier_rename_20261112_group_configs s)
          UNION ALL
          (SELECT to_jsonb(s) FROM tier_rename_20261112_group_configs s EXCEPT SELECT to_jsonb(g) FROM group_configs g WHERE group_key = ANY(old_keys))
       ) OR coalesce((SELECT value::jsonb FROM options WHERE key = 'GroupRatio'), '{}'::jsonb) IS DISTINCT FROM restored_ratio THEN
        RAISE EXCEPTION 'Rollback assertions failed';
    END IF;
    UPDATE tier_rename_20261112_state SET status = 'rolled_back' WHERE status = 'applied';
    RAISE NOTICE 'Rollback assertions passed; restored users=%, reverse-mapped users=%. Custom groups and current ability attributes preserved; snapshots retained', restored, reverse_count;
END
$tier_rollback$;

ROLLBACK;
