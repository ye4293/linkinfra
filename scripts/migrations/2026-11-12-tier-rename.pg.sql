-- 仅供人工审核后执行；默认演练。实际提交只改最后一行。
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';
SET LOCAL search_path = public, pg_catalog;

DO $tier_rename$
DECLARE
    -- 参数：用户已确认按旧 key 合并、不降级。
    mapping jsonb := '{"Lv1":"Start","Lv2":"Build","Lv3":"Build","Lv4":"Scale","Lv5":"Scale","Lv6":"Enterprise"}';
    -- 参数：to 拥有 from 的全部渠道；空数组关闭，多个关系按数组顺序应用至稳定。
    inherits jsonb := '[{"from":"Scale","to":"Enterprise"}]';
    -- 参数：美元门槛、顺序、继承源；null 表示不覆盖折扣或返现。
    definitions jsonb := '[
      {"key":"Start",      "dollars":0,    "sort_order":0,"inherit":"Lv1","discount_override":null,"commission_override":null},
      {"key":"Build",      "dollars":50,   "sort_order":1,"inherit":"Lv3","discount_override":null,"commission_override":null},
      {"key":"Scale",      "dollars":300,  "sort_order":2,"inherit":"Lv5","discount_override":null,"commission_override":null},
      {"key":"Enterprise", "dollars":1000, "sort_order":3,"inherit":"Lv6","discount_override":null,"commission_override":null}
    ]';
    units numeric;
    ratios jsonb;
    resolved jsonb;
    seen text[];
    old_keys text[];
    new_keys text[];
    bad_ids text;
    item record;
    total_before bigint;
    ability_before bigint;
    routing jsonb;
    changed_count bigint;
    pass_changes bigint;
BEGIN
    IF pg_is_in_recovery() THEN
        RAISE EXCEPTION 'Connected to a read-only standby; reconnect to the primary before running this script';
    END IF;
    FOREACH bad_ids IN ARRAY ARRAY['users','channels','abilities','group_configs','options'] LOOP
        IF to_regclass(bad_ids) IS NULL THEN
            RAISE EXCEPTION 'Required table is missing: %', bad_ids;
        END IF;
    END LOOP;
    -- 阻止事务中的并发写入；仍不能消除应用在事务外先读配置再写用户的窗口。
    LOCK TABLE users, channels, abilities, group_configs, options IN SHARE ROW EXCLUSIVE MODE;

    IF jsonb_typeof(mapping) <> 'object' OR jsonb_typeof(definitions) <> 'array'
       OR jsonb_array_length(definitions) <> 4 THEN
        RAISE EXCEPTION 'Invalid mapping or tier definitions';
    END IF;
    SELECT array_agg(key ORDER BY key) INTO old_keys FROM jsonb_each_text(mapping);
    SELECT array_agg(d.key ORDER BY d.key) INTO new_keys
    FROM jsonb_to_recordset(definitions) AS d(key text);
    IF new_keys IS DISTINCT FROM ARRAY['Build','Enterprise','Scale','Start']::text[]
       OR old_keys IS NULL OR old_keys && new_keys
       OR EXISTS (SELECT 1 FROM jsonb_each_text(mapping) m
                  WHERE btrim(m.key) <> m.key OR m.key = '' OR length(m.key) > 32
                     OR position(',' in m.key) > 0 OR NOT (m.value = ANY(new_keys)) OR m.value IS NULL)
       OR EXISTS (SELECT 1 FROM jsonb_to_recordset(definitions) AS d(key text, dollars numeric, sort_order int, inherit text, discount_override numeric, commission_override numeric)
                  WHERE d.dollars IS NULL OR d.dollars < 0 OR d.dollars::text IN ('NaN','Infinity','-Infinity')
                     OR d.sort_order IS NULL OR mapping->>d.inherit IS DISTINCT FROM d.key
                     OR d.discount_override < 0 OR d.discount_override::text IN ('NaN','Infinity','-Infinity')
                     OR d.commission_override NOT BETWEEN 0 AND 1 OR d.commission_override::text IN ('NaN','Infinity','-Infinity'))
       OR (SELECT count(DISTINCT d.sort_order) FROM jsonb_to_recordset(definitions) AS d(sort_order int)) <> 4 THEN
        RAISE EXCEPTION 'Invalid keys, inheritance, thresholds, ordering or overrides';
    END IF;
    IF jsonb_typeof(inherits) IS DISTINCT FROM 'array' THEN
        RAISE EXCEPTION 'inherits must be an array';
    END IF;
    IF EXISTS (SELECT 1 FROM jsonb_to_recordset(inherits) AS e("from" text, "to" text)
               WHERE e."from" IS NULL OR e."to" IS NULL OR NOT (e."from" = ANY(new_keys))
                  OR NOT (e."to" = ANY(new_keys)) OR e."from" = e."to") THEN
        RAISE EXCEPTION 'Invalid inherits endpoints: use two different new tier keys';
    END IF;
    -- 有限集合的传递闭包让链式或环状关系也满足每条包含断言；UNION 消除重复。
    WITH RECURSIVE reach(src, dst) AS (
        SELECT k, k FROM unnest(new_keys) k
        UNION
        SELECT r.src, e."to" FROM reach r
        JOIN jsonb_to_recordset(inherits) AS e("from" text, "to" text) ON e."from" = r.dst
    )
    SELECT jsonb_object_agg(src, targets) INTO routing
    FROM (SELECT src, jsonb_agg(dst ORDER BY dst) targets FROM reach GROUP BY src) r;

    SELECT coalesce((SELECT btrim(value)::numeric FROM options WHERE key = 'QuotaPerUnit'), 500000) INTO units;
    IF units <= 0 OR units::text IN ('NaN','Infinity','-Infinity') THEN
        RAISE EXCEPTION 'Invalid QuotaPerUnit: %', units;
    END IF;
    IF EXISTS (SELECT 1 FROM jsonb_to_recordset(definitions) AS d(dollars numeric)
               WHERE d.dollars * units <> floor(d.dollars * units) OR d.dollars * units > 9223372036854775807) THEN
        RAISE EXCEPTION 'Tier thresholds must fit integral bigint quota';
    END IF;
    SELECT value::jsonb INTO ratios FROM options WHERE key = 'GroupRatio';
    IF FOUND AND (ratios IS NULL OR jsonb_typeof(ratios) <> 'object') THEN
        RAISE EXCEPTION 'GroupRatio must be a JSON object';
    END IF;
    ratios := coalesce(ratios, '{}'::jsonb);
    IF EXISTS (SELECT 1 FROM jsonb_each(ratios) r WHERE jsonb_typeof(r.value) <> 'number') THEN
        RAISE EXCEPTION 'GroupRatio contains a nonnumeric multiplier';
    END IF;
    IF EXISTS (SELECT 1 FROM jsonb_each_text(ratios) r WHERE r.value::numeric < 0) THEN
        RAISE EXCEPTION 'GroupRatio contains a negative multiplier';
    END IF;

    SELECT array_agg(DISTINCT k) INTO seen FROM (
        SELECT "group" AS k FROM users
        UNION ALL SELECT btrim(s.k) FROM channels c CROSS JOIN LATERAL unnest(string_to_array(c."group", ',')) s(k)
        UNION ALL SELECT "group" FROM abilities
        UNION ALL SELECT group_key FROM group_configs
        UNION ALL SELECT jsonb_object_keys(ratios)
    ) keys;
    IF seen && old_keys AND seen && new_keys THEN
        RAISE EXCEPTION 'Mixed old and new tier keys found; reconcile all five stores before retrying';
    END IF;
    IF NOT coalesce(seen && old_keys, false) THEN
        IF EXISTS (SELECT 1 FROM unnest(new_keys) k WHERE NOT EXISTS (SELECT 1 FROM group_configs g WHERE g.group_key = k) OR NOT (ratios ? k)) THEN
            RAISE EXCEPTION 'No old keys found, but the four new tiers are incomplete';
        END IF;
        RAISE NOTICE 'Already migrated: no changes made';
        RETURN;
    END IF;

    SELECT jsonb_agg(jsonb_build_object(
        'key', d.key, 'sort_order', d.sort_order, 'inherit', d.inherit,
        'threshold', (d.dollars * units)::bigint,
        'discount', coalesce(d.discount_override, (ratios->>d.inherit)::numeric, g.discount, 1),
        'commission', coalesce(d.commission_override, g.commission_rate, 0),
        'id', coalesce(g.id, least((SELECT coalesce(min(id), 0) FROM group_configs), 0) - d.rn)
    ) ORDER BY d.sort_order) INTO resolved
    FROM (SELECT d.*, row_number() OVER (ORDER BY d.sort_order) rn
          FROM jsonb_to_recordset(definitions) AS d(key text, dollars numeric, sort_order int, inherit text, discount_override numeric, commission_override numeric)) d
    LEFT JOIN group_configs g ON g.group_key = d.inherit;

    FOR item IN SELECT g.group_key, g.discount AS configured, r.value::numeric AS effective
                FROM group_configs g JOIN jsonb_each_text(ratios) r ON r.key = g.group_key
                WHERE g.discount IS DISTINCT FROM r.value::numeric LOOP
        RAISE NOTICE 'Multiplier mismatch: key=%, config=%, effective=%', item.group_key, item.configured, item.effective;
    END LOOP;
    -- 较高旧等级的倍率不一定较低；默认继承如果涨价则中止，显式覆盖表示人工已决定。
    FOR item IN
        SELECT m.key, m.value, coalesce((ratios->>m.key)::numeric, g.discount, 1) old_ratio, d.discount new_ratio
        FROM jsonb_each_text(mapping) m
        LEFT JOIN group_configs g ON g.group_key = m.key
        JOIN jsonb_to_recordset(resolved) AS d(key text, discount numeric) ON d.key = m.value
        JOIN jsonb_to_recordset(definitions) AS p(key text, discount_override numeric) ON p.key = d.key
        WHERE d.discount > coalesce((ratios->>m.key)::numeric, g.discount, 1) AND p.discount_override IS NULL
    LOOP
        RAISE EXCEPTION 'Inherited multiplier increases price: % -> %, % -> %; review an explicit override', item.key, item.value, item.old_ratio, item.new_ratio;
    END LOOP;

    -- 固定快照名不能覆盖；回滚后需要重新迁移时，由人工归档并更换整套前缀。
    FOREACH bad_ids IN ARRAY ARRAY['state','users','channels','abilities','group_configs','options','abilities_after','group_configs_after','options_after'] LOOP
        IF to_regclass('tier_rename_20261112_' || bad_ids) IS NOT NULL THEN
            RAISE EXCEPTION 'Snapshot already exists: tier_rename_20261112_%', bad_ids;
        END IF;
    END LOOP;
    SELECT count(*) INTO total_before FROM users;
    SELECT count(*) INTO ability_before FROM abilities;
    CREATE TABLE tier_rename_20261112_state AS
        SELECT mapping AS key_mapping, definitions AS definitions, resolved AS resolved, inherits AS inherits,
               units AS quota_per_unit, total_before AS total_users, 'applied'::text AS status;
    -- 只保存修改所需字段，不保存用户个人资料；使用 id 避开外部清库脚本的保留列名。
    CREATE TABLE tier_rename_20261112_users AS
        SELECT id, "group", topup_quota, mapping->>"group" AS mapped_group FROM users WHERE mapping ? "group";
    CREATE TABLE tier_rename_20261112_channels AS
        SELECT c.id, c."group", c.status, changed.mapped AS base_group, changed.mapped AS mapped_group
        FROM channels c CROSS JOIN LATERAL (
            SELECT string_agg(k, ',' ORDER BY first_pos) AS mapped
            FROM (SELECT coalesce(mapping->>btrim(s.k), btrim(s.k)) AS k, min(s.pos) AS first_pos
                  FROM unnest(string_to_array(c."group", ',')) WITH ORDINALITY s(k,pos)
                  WHERE btrim(s.k) <> '' GROUP BY 1) dedup
        ) changed
        WHERE EXISTS (SELECT 1 FROM unnest(string_to_array(c."group", ',')) s(k) WHERE mapping ? btrim(s.k));
    CREATE TABLE tier_rename_20261112_abilities AS SELECT * FROM abilities WHERE mapping ? "group";
    CREATE TABLE tier_rename_20261112_group_configs AS SELECT * FROM group_configs WHERE mapping ? group_key;
    CREATE TABLE tier_rename_20261112_options AS SELECT key, value FROM options WHERE key = 'GroupRatio';

    -- 每次只插入缺少的目标，保留已有项位置；每轮最多补齐有限的四档。
    LOOP
        pass_changes := 0;
        FOR item IN SELECT * FROM jsonb_to_recordset(inherits) AS e("from" text, "to" text) LOOP
            UPDATE tier_rename_20261112_channels c SET mapped_group = array_to_string(
                (string_to_array(c.mapped_group, ','))[1:array_position(string_to_array(c.mapped_group, ','), item."from")]
                || ARRAY[item."to"] ||
                (string_to_array(c.mapped_group, ','))[array_position(string_to_array(c.mapped_group, ','), item."from")+1:cardinality(string_to_array(c.mapped_group, ','))], ',')
            WHERE item."from" = ANY(string_to_array(c.mapped_group, ','))
              AND NOT (item."to" = ANY(string_to_array(c.mapped_group, ',')));
            GET DIAGNOSTICS changed_count = ROW_COUNT;
            pass_changes := pass_changes + changed_count;
        END LOOP;
        EXIT WHEN pass_changes = 0;
    END LOOP;
    SELECT string_agg(id::text, ', ' ORDER BY id) INTO bad_ids
    FROM tier_rename_20261112_channels WHERE length(mapped_group) > 32;
    IF bad_ids IS NOT NULL THEN
        RAISE EXCEPTION 'Mapped channels.group exceeds 32 characters after inheritance; channel ids: %', bad_ids;
    END IF;
    -- 先保存包含继承的预期结果，写入后用双向差集核对，回滚也以此核对结构。
    CREATE TABLE tier_rename_20261112_abilities_after AS
        SELECT target.k AS "group", a.model, a.channel_id, bool_or(a.enabled) AS enabled, max(a.priority) AS priority
        FROM tier_rename_20261112_abilities a
        CROSS JOIN LATERAL jsonb_array_elements_text(routing->(mapping->>a."group")) target(k)
        GROUP BY 1,2,3;

    RAISE NOTICE 'QuotaPerUnit: %, total users: %, abilities before: %', units, total_before, ability_before;
    FOR item IN
        SELECT m.key, m.value, count(u.id) AS users, count(u.id) FILTER (WHERE coalesce(u.topup_quota,0) < d.threshold) AS below_threshold
        FROM jsonb_each_text(mapping) m
        JOIN jsonb_to_recordset(resolved) AS d(key text, threshold bigint) ON d.key = m.value
        LEFT JOIN tier_rename_20261112_users u ON u."group" = m.key
        GROUP BY m.key, m.value ORDER BY m.key
    LOOP
        RAISE NOTICE 'Before: % -> %, users=%, below mapped threshold=%', item.key, item.value, item.users, item.below_threshold;
    END LOOP;
    RAISE NOTICE 'Unmapped/custom keys preserved: %', (SELECT jsonb_agg(k ORDER BY k) FROM unnest(seen) k WHERE k <> '' AND NOT (k = ANY(old_keys)));
    RAISE NOTICE 'All user groups before: %', (SELECT jsonb_object_agg(k, n) FROM (SELECT coalesce("group", '<NULL>') k, count(*) n FROM users GROUP BY 1) counts);
    FOR item IN SELECT "group", mapped_group, count(*) AS channels,
                       count(*) FILTER (WHERE status = 1) AS enabled_channels, (array_agg(id ORDER BY id))[1:5] AS sample_ids
                FROM tier_rename_20261112_channels GROUP BY "group", mapped_group ORDER BY "group", mapped_group LOOP
        RAISE NOTICE 'Channels: % -> %, count=%, enabled=%, sample ids=%', item."group", item.mapped_group, item.channels, item.enabled_channels, item.sample_ids;
    END LOOP;
    RAISE NOTICE 'Channels supplemented by inheritance: %', (SELECT count(*) FROM tier_rename_20261112_channels WHERE mapped_group <> base_group);
    FOR item IN SELECT target, (SELECT count(*) FROM tier_rename_20261112_channels c
                                WHERE target = ANY(string_to_array(c.mapped_group, ','))
                                  AND NOT (target = ANY(string_to_array(c.base_group, ',')))) AS channels
                FROM (SELECT DISTINCT e."to" AS target FROM jsonb_to_recordset(inherits) AS e("to" text)) targets ORDER BY target LOOP
        RAISE NOTICE 'Channels supplemented for %: %', item.target, item.channels;
    END LOOP;
    RAISE NOTICE 'group_configs before: %', (SELECT jsonb_agg(to_jsonb(g) ORDER BY sort_order, id) FROM group_configs g);
    RAISE NOTICE 'GroupRatio before (row absent = SQL NULL): %', (SELECT value FROM tier_rename_20261112_options);
    RAISE NOTICE 'Resolved tiers: %', resolved;
    FOR item IN
        SELECT d.key,
            (SELECT count(*) FROM tier_rename_20261112_abilities a WHERE routing->(mapping->>a."group") ? d.key) AS source_rows,
            (SELECT count(*) FROM tier_rename_20261112_abilities_after a WHERE a."group" = d.key) AS merged_rows,
            (SELECT count(*) FROM tier_rename_20261112_abilities_after a WHERE a."group" = d.key AND NOT EXISTS (
                SELECT 1 FROM tier_rename_20261112_abilities b WHERE mapping->>b."group" = d.key AND b.model = a.model AND b.channel_id = a.channel_id)) AS inherited_rows
        FROM jsonb_to_recordset(resolved) AS d(key text, sort_order int) ORDER BY d.sort_order
    LOOP
        RAISE NOTICE 'Abilities for %: source rows=%, merged rows=%, added by inheritance=%', item.key, item.source_rows, item.merged_rows, item.inherited_rows;
    END LOOP;

    UPDATE users u SET "group" = s.mapped_group FROM tier_rename_20261112_users s WHERE u.id = s.id;
    UPDATE channels c SET "group" = s.mapped_group FROM tier_rename_20261112_channels s WHERE c.id = s.id;
    INSERT INTO abilities ("group", model, channel_id, enabled, priority)
        SELECT * FROM tier_rename_20261112_abilities_after;
    DELETE FROM abilities WHERE "group" = ANY(old_keys);

    -- 先删旧配置再插入新配置，复用继承源的 id，不调用序列；演练不会消耗序列值。
    -- 继承源缺失时使用小于现有最小 id 的负数，后续后台自增 id 不会冲突。
    DELETE FROM group_configs WHERE group_key = ANY(old_keys);
    INSERT INTO group_configs (id, group_key, display_name, discount, sort_order, description, commission_rate, upgrade_threshold)
        SELECT d.id, d.key, d.key, d.discount, d.sort_order, '', d.commission, d.threshold
        FROM jsonb_to_recordset(resolved) AS d(id bigint, key text, discount numeric, sort_order int, commission numeric, threshold bigint);
    INSERT INTO options (key, value)
        SELECT 'GroupRatio', ((ratios - old_keys) || (SELECT jsonb_object_agg(d.key, d.discount) FROM jsonb_to_recordset(resolved) AS d(key text, discount numeric)))::text
        ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;

    IF EXISTS (SELECT 1 FROM users WHERE "group" = ANY(old_keys))
       OR EXISTS (SELECT 1 FROM channels c CROSS JOIN LATERAL unnest(string_to_array(c."group", ',')) s(k) WHERE btrim(s.k) = ANY(old_keys))
       OR EXISTS (SELECT 1 FROM abilities WHERE "group" = ANY(old_keys))
       OR EXISTS (SELECT 1 FROM group_configs WHERE group_key = ANY(old_keys))
       OR EXISTS (SELECT 1 FROM options WHERE key = 'GroupRatio' AND value::jsonb ?| old_keys) THEN
        RAISE EXCEPTION 'Assertion failed: old keys remain';
    END IF;
    IF (SELECT count(*) FROM users) <> total_before
       OR EXISTS (SELECT 1 FROM unnest(new_keys) k
                  WHERE (SELECT count(*) FROM users WHERE "group" = k) <> (SELECT count(*) FROM tier_rename_20261112_users WHERE mapped_group = k))
       OR EXISTS (SELECT 1 FROM tier_rename_20261112_users s JOIN users u ON u.id = s.id WHERE u."group" IS DISTINCT FROM s.mapped_group) THEN
        RAISE EXCEPTION 'Assertion failed: user totals or mapped groups differ';
    END IF;
    IF EXISTS (
        (SELECT * FROM tier_rename_20261112_abilities_after
         EXCEPT SELECT "group", model, channel_id, enabled, priority FROM abilities WHERE "group" = ANY(new_keys))
        UNION ALL
        (SELECT "group", model, channel_id, enabled, priority FROM abilities WHERE "group" = ANY(new_keys)
         EXCEPT SELECT * FROM tier_rename_20261112_abilities_after)
    ) THEN
        RAISE EXCEPTION 'Assertion failed: mapped ability sets or attributes differ';
    END IF;
    FOR item IN SELECT * FROM jsonb_to_recordset(inherits) AS e("from" text, "to" text) LOOP
        IF EXISTS (SELECT model, channel_id FROM abilities WHERE "group" = item."from"
                   EXCEPT SELECT model, channel_id FROM abilities WHERE "group" = item."to")
           OR EXISTS (SELECT 1 FROM channels WHERE item."from" = ANY(string_to_array("group", ','))
                       AND NOT (item."to" = ANY(string_to_array("group", ',')))) THEN
            RAISE EXCEPTION 'Assertion failed: % does not contain all channels/models of %', item."to", item."from";
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM tier_rename_20261112_channels s LEFT JOIN channels c ON c.id = s.id WHERE c."group" IS DISTINCT FROM s.mapped_group)
       OR EXISTS (SELECT 1 FROM jsonb_to_recordset(resolved) AS d(key text, threshold bigint, discount numeric, sort_order int)
                  LEFT JOIN group_configs g ON g.group_key = d.key
                  WHERE g.upgrade_threshold IS DISTINCT FROM d.threshold OR g.sort_order IS DISTINCT FROM d.sort_order OR g.display_name IS DISTINCT FROM d.key
                     OR (SELECT (value::jsonb->>d.key)::numeric FROM options WHERE key = 'GroupRatio') IS DISTINCT FROM d.discount) THEN
        RAISE EXCEPTION 'Assertion failed: channels, tier definitions or multipliers differ';
    END IF;
    CREATE TABLE tier_rename_20261112_group_configs_after AS SELECT * FROM group_configs WHERE group_key = ANY(new_keys);
    CREATE TABLE tier_rename_20261112_options_after AS SELECT key, value FROM options WHERE key = 'GroupRatio';

    RAISE NOTICE 'All user groups after: %', (SELECT jsonb_object_agg(k, n) FROM (SELECT coalesce("group", '<NULL>') k, count(*) n FROM users GROUP BY 1) counts);
    FOR item IN SELECT d.key, count(u.id) AS people
                FROM jsonb_to_recordset(resolved) AS d(key text, sort_order int)
                LEFT JOIN users u ON u."group" = d.key GROUP BY d.key, d.sort_order ORDER BY d.sort_order LOOP
        RAISE NOTICE 'After: %, users=%', item.key, item.people;
    END LOOP;
    RAISE NOTICE 'Abilities: % -> %', ability_before, (SELECT count(*) FROM abilities);
    RAISE NOTICE 'group_configs after: %', (SELECT jsonb_agg(to_jsonb(g) ORDER BY sort_order, id) FROM group_configs g);
    RAISE NOTICE 'GroupRatio after: %', (SELECT value FROM options WHERE key = 'GroupRatio');
    RAISE NOTICE 'Assertions passed. The final transaction command determines whether this is a rehearsal or a commit';
END
$tier_rename$;

ROLLBACK;
