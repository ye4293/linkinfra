# 四档等级迁移上线手册

适用：PostgreSQL、`public` schema、先发兼容代码再迁移数据。脚本的开发和自动验证只用了内存 SQLite 和 PGlite。之后经用户同意，在线上库做了只读检查和两次以 `ROLLBACK` 结束的演练，结果见第 6 节。提交迁移前需要用户再次确认。

## 1. 发布前检查

在生产副本先执行以下只读查询，再对生产执行同样的只读检查并保存输出：

```sql
SELECT key, value FROM options WHERE key IN ('GroupRatio', 'QuotaPerUnit') ORDER BY key;
SELECT group_key, display_name, discount, commission_rate, upgrade_threshold, sort_order
FROM group_configs ORDER BY upgrade_threshold, sort_order, id;
SELECT "group", count(*) AS people, min(topup_quota), max(topup_quota)
FROM users GROUP BY "group" ORDER BY "group";
SELECT g.group_key, g.discount AS configured,
       (o.value::jsonb ->> g.group_key)::numeric AS effective
FROM group_configs g LEFT JOIN options o ON o.key = 'GroupRatio'
ORDER BY g.sort_order, g.id;
SELECT column_default, character_maximum_length FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'group';
```

确认旧配置仍完整、入门档按 `upgrade_threshold, sort_order, id` 排序得到预期结果，`GroupRatio` 是数值对象，`QuotaPerUnit` 为正数。没有 `QuotaPerUnit` 行时迁移使用 500000；非法值不猜测，脚本中止。组配置显示折扣与实际倍率不一致时，以 `options.GroupRatio` 为计费依据。

**如果没有 `GroupRatio` 行，必须在发布新代码前补齐。** 旧二进制启动后的编译默认值是六个 Lv key、倍率全部为 1；不能用 `group_configs.discount` 推断实际计费值。若该行在运行中被删过，各实例可能保留旧内存值：通过 root 管理接口 `GET /api/option/` 核对每个实例返回的 `GroupRatio`，确认一致后保存那份值。此接口可能返回其他管理配置，记录时只保留所需字段。

仅在确认实际生效值是旧默认六档全 1 时，人工执行以下补齐事务。若有效值不同，替换 JSON 为核对得到的完整对象（包括自定义 key），先演练再提交：

```sql
BEGIN;
INSERT INTO options (key, value)
SELECT 'GroupRatio', '{"Lv1":1,"Lv2":1,"Lv3":1,"Lv4":1,"Lv5":1,"Lv6":1}'
WHERE NOT EXISTS (SELECT 1 FROM options WHERE key = 'GroupRatio');
SELECT key, value FROM options WHERE key = 'GroupRatio';
ROLLBACK;
```

实际执行把上例最后一行改为 `COMMIT`，随后确认所有实例的有效倍率一致，才开始发布兼容代码。新代码上线后、数据迁移前，旧配置和倍率继续生效。`users.group` 的原默认 tag 保留，不会因本次 tag 变更触发列 DDL；创建用户由 GORM 钩子选入门档。**直接 SQL、跳过钩子的导入必须显式填写 group**，否则数据库保留的默认值仍是 `Lv1`。

文件容量有两项预先约定的变化：默认门槛下 Lv6 从一律拒绝变为 2GB，未知分组从一律拒绝变为 100MB；默认旧门槛下 Lv1–Lv5 保持原容量。门槛被改过的库，旧等级的上限会按各自的门槛重新分档；发布代码后、迁移前可能变大，用户已接受这一行为。生产库的门槛不是默认值。新四档依次为 100MB、500MB、2GB、2GB。分组下拉按配置排序，集合仍是生效的内存倍率 key。

## 2. 备份与副本演练

没有独立副本时，可以直接在线上库演练：脚本默认以 `ROLLBACK` 结尾，不会留下改动。演练期间五张表的写入会等待几秒，读不受影响；回滚后会留下死元组，`abilities` 和 `channels` 暂时变大，自动清理后空间可复用。演练前先把会改到的数据导出一份，演练后核对数据没有变化。

1. 做可恢复的完整备份，验证恢复能力。快照只记录本次改动需要的字段，不能代替完整备份。
2. 在生产副本上使用已发布的同一版代码，确认全部实例完成滚动更新。旧代码不能与已迁移数据库混用。
3. 用户已确认按等级名合并、不降级：Lv1→Start，Lv2/Lv3→Build，Lv4/Lv5→Scale，Lv6→Enterprise。这保留旧 key 对应的层级，部分老用户会保留高于充值额所能达到的档位。执行前仍需核对 SQL 顶部参数与已确认的方案一致。
4. 四档门槛是 $0/$50/$300/$1000，乘以数据库中的 `QuotaPerUnit`。默认继承源是 Lv1/Lv3/Lv5/Lv6；继承源必须属于映射到当前新档的旧 key，调整映射时须同步核对继承源。折扣按有效 GroupRatio → 配置 discount → 1，返现按源配置 → 0。`discount_override`、`commission_override` 为 null 时继承，否则使用显式值。缺少倍率行的场景也能演练，但生产发布前仍需完成上一节的补齐。
5. 较高旧等级的倍率可能被配置得更贵，不能仅凭等级名保证不涨价。默认继承会使任一旧组倍率变大时脚本中止；请检查报告并将显式覆盖值设置为已确认的倍率。显式覆盖可以改变价格，必须人工审核。
   用户同时已确认 Enterprise 拥有 Scale 的全部渠道。`inherits` 默认 `[{"from":"Scale","to":"Enterprise"}]`，空数组关闭；端点必须是两个不同的新档。映射、去重后，缺少的 to 插入 from 后；已有 to 不移动。多个关系按数组顺序反复应用直到稳定，abilities 按同一传递闭包汇集旧行，再取 bool_or(enabled)、max(priority)。这也支持链式或环状关系，最多补齐四档。长度检查在继承完成后执行。参数随快照保存，回滚删除继承带来的新行。
6. 运行脚本并保存包含 NOTICE 的完整输出。例如在已配置好数据库连接的终端中：

```sh
psql -X -v ON_ERROR_STOP=1 -f scripts/migrations/2026-11-12-tier-rename.pg.sql > tier-rename-rehearsal.log 2>&1
```

也可以使用任何能把整份文件原样作为一次请求发送的客户端，例如 node 的 pg 驱动。不要拆分语句、使用多个连接或只发送 DO 块；两份脚本都在 DO 开头检查 `pg_is_in_recovery()`，连到只读节点会中止。连接必须由代理路由到主节点，表锁保持允许普通读取的 `SHARE ROW EXCLUSIVE`。

脚本默认最后一行为 `ROLLBACK`。执行后检查客户端退出码、最后的断言成功提示及回滚状态。出错时必须结束失败事务后才能继续操作；不要在失败事务中补执行提交。默认锁等待超时 5 秒，语句超时 120 秒；生产副本演练若显示数据量较大，应先优化/安排维护窗口，再人工调整超时。

报告包括每个旧 key 的人数（包括 0 人）、映射后未达门槛人数、全部用户分布、自定义 key、配置和倍率前后对比及两者原先不一致的记录。渠道按“原列表→最终列表”分组，列渠道数、启用数（status=1）及最多 5 个示例 ID；abilities 每个新档一行，列包含继承来源的合并前行数、合并后行数及因继承新增的行数，已有目标行只合并属性，不计作新增。另列因继承补齐的渠道总数。用户统计只报人数，不输出用户名或邮箱。审核演练结果和初始价格后才能提交。

若渠道映射后超过 varchar(32)，脚本列出渠道 ID 并中止。先人工调整该渠道的分组安排或拆分渠道，再重新演练；不要加宽列。未含旧 key 的渠道保持原字符串，含旧 key 的渠道才去空格、空项、映射和去重。

## 3. 生产执行与缓存

建议暂停全部后端实例并等待在途请求、充值回调完成，再执行迁移。数据库表锁只能保证事务内数据一致，无法覆盖 `RecalcUserLevel` 在应用层“读配置、写用户”的完整时间段。不停服迁移时，个别请求可能在提交后又写回旧 key。

1. 再确认备份、副本演练报告、映射、折扣和返现参数，并确认全部实例已使用兼容代码。
2. 保留原始演练脚本，复制一份已审核的执行稿，只把最后一行改为 `COMMIT`。不要分段运行 SQL，也不要改动断言。参数或生产数据变化后应重新演练。
3. 执行整份脚本，保存报告。脚本将五处改动和快照放在一个事务内；旧新 key 混杂、渠道超长、参数非法或断言失败都会整体中止。完全迁移过的数据库提示 `Already migrated` 并不做改动。
4. **提交后删除同一 Redis 实例、同一逻辑库中的全部 `user_group:*` key。** 使用 SCAN 分批遍历，再对得到的真实 key 调用 UNLINK 或 DEL，直到游标回到 0；DEL 的字符串参数不会展开通配符。不要清空整个 Redis 库。在停服期间清理，防止旧请求把缓存写回来。
5. **重启全部后端实例**，重新载入渠道缓存和分组倍率，再恢复流量。只等自动同步或只重启部分实例都不够；旧缓存最长可持续 `SYNC_FREQUENCY` 秒（默认 600 秒），期间可能找不到可用渠道。
6. 运行下一节的核对语句，抽查四档账户可用模型和请求计费，确认注册默认 Start、分组下拉顺序正确。
7. 迁移完成后由用户到后台重新配置四档折扣与返现。回滚支持这种变更：先报告当前值，再恢复旧配置。确认迁移没有问题之前，不要新增、删除或编辑渠道；渠道列表和 ability 结构变化仍需人工处理，正常自动启停和优先级变化允许回滚。

## 4. 提交后核对及在线迁移补救

以下旧 key 检查应返回 0 行。历史返现快照和日志有旧名字是正常的，不在检查范围内：

```sql
WITH old AS (SELECT jsonb_object_keys(key_mapping) AS k FROM tier_rename_20261112_state),
hits AS (
  SELECT 'users' AS source, id::text AS ref, "group" AS k FROM users
  UNION ALL SELECT 'channels', c.id::text, btrim(s.k) FROM channels c
    CROSS JOIN LATERAL unnest(string_to_array(c."group", ',')) s(k)
  UNION ALL SELECT 'abilities', channel_id::text || ':' || model, "group" FROM abilities
  UNION ALL SELECT 'group_configs', id::text, group_key FROM group_configs
  UNION ALL SELECT 'options', o.key, r.k FROM options o
    CROSS JOIN LATERAL jsonb_object_keys(o.value::jsonb) r(k) WHERE o.key='GroupRatio'
)
SELECT hits.* FROM hits JOIN old USING (k) ORDER BY source, ref;

SELECT group_key, display_name, upgrade_threshold, sort_order, discount, commission_rate
FROM group_configs ORDER BY sort_order, id;
SELECT key, value FROM options WHERE key='GroupRatio';
SELECT count(*) AS current_users, (SELECT total_users FROM tier_rename_20261112_state) AS before_users FROM users;
SELECT s.mapped_group, count(*) AS before_people,
       (SELECT count(*) FROM users u WHERE u."group"=s.mapped_group) AS current_people
FROM tier_rename_20261112_users s GROUP BY s.mapped_group ORDER BY s.mapped_group;
SELECT c.id, s."group" AS before_groups, s.mapped_group, c."group" AS current_groups
FROM tier_rename_20261112_channels s JOIN channels c ON c.id=s.id ORDER BY c.id;
WITH RECURSIVE reach(src, dst) AS (
  SELECT DISTINCT m.value, m.value FROM tier_rename_20261112_state st CROSS JOIN LATERAL jsonb_each_text(st.key_mapping) m
  UNION
  SELECT r.src, e."to" FROM reach r CROSS JOIN tier_rename_20261112_state st
    CROSS JOIN LATERAL jsonb_to_recordset(st.inherits) AS e("from" text, "to" text) WHERE e."from"=r.dst
), expected AS (
  SELECT r.dst AS "group", a.model, a.channel_id, bool_or(a.enabled) AS enabled, max(a.priority) AS priority
  FROM tier_rename_20261112_abilities a CROSS JOIN tier_rename_20261112_state st
  JOIN reach r ON r.src=st.key_mapping->>a."group" GROUP BY 1,2,3
), actual AS (
  SELECT a.* FROM abilities a WHERE a."group" IN (SELECT DISTINCT value FROM tier_rename_20261112_state st CROSS JOIN LATERAL jsonb_each_text(st.key_mapping))
)
(SELECT * FROM expected EXCEPT SELECT * FROM actual)
UNION ALL
(SELECT * FROM actual EXCEPT SELECT * FROM expected);
```

恢复流量前人数应精确吻合，ability 差集应为空。恢复后新增用户、升档或编辑渠道可能造成合理变化，应结合迁移时间核对。

若只有用户被并发请求写回旧 key，**先停服并排空在途请求**，查询并审核这些用户，再执行以下修正演练。修正选择“当前旧 key 的映射、迁移快照保留的档位、充值够到的新档”中门槛最高者，避免修正时把老用户保留的档位降回去：

```sql
BEGIN;
SET LOCAL lock_timeout='5s';
SET LOCAL statement_timeout='120s';
WITH fixes AS (
  SELECT u.id, u."group" AS old_group,
    (SELECT g.group_key FROM group_configs g
     WHERE g.group_key IN (SELECT value FROM jsonb_each_text(st.key_mapping))
       AND (g.group_key IN (st.key_mapping->>u."group", s.mapped_group)
            OR g.upgrade_threshold <= coalesce(u.topup_quota,0))
     ORDER BY g.upgrade_threshold DESC, g.sort_order, g.id LIMIT 1) AS target
  FROM users u CROSS JOIN tier_rename_20261112_state st
  LEFT JOIN tier_rename_20261112_users s ON s.id=u.id
  WHERE st.status='applied' AND st.key_mapping ? u."group"
)
UPDATE users u SET "group"=f.target FROM fixes f
WHERE u.id=f.id AND u."group"=f.old_group AND f.target IS NOT NULL
RETURNING u.id, f.old_group, u."group";
ROLLBACK;
```

审核返回的用户后将最后一行改为 `COMMIT` 执行，再次复查五处旧 key、清 Redis 分组缓存、重启全部实例。如果发现渠道、配置或倍率被旧代码写回，不要用这条用户修正处理，应保持停服并按备份和演练报告人工核对。

## 5. 回滚

使用 `2026-11-12-tier-rename.rollback.pg.sql`，先对副本演练，再停服、审核输出、将最后一行由 `ROLLBACK` 改为 `COMMIT` 后提交。

- 快照为 `tier_rename_20261112_*` 九张表。只保存受影响的用户 id/group/topup_quota、渠道原列表/状态/映射及继承后的列表、旧 abilities/config/option，以及包含 inherits 的参数和迁移后状态；没有保留列名 user_id/inviter_id/invitee_id/token_id。历史返现和日志不改。
- 配置复用继承源的 ID；为保证演练不消耗序列，配置在同一事务内先删除旧行、再插入新行。继承源缺失时使用未占用的负 ID，正常自增不会冲突。回滚恢复原 ID 和原值，快照不会删除。
- 当前 group 仍等于快照映射值的用户恢复快照旧 group；新注册、已升档或被管理员改到其他新档的用户，按回滚顶部 `reverse_mapping` 恢复：Start→Lv1、Build→Lv2、Scale→Lv4、Enterprise→Lv6。每档默认取对应的最低旧 key；之后充值时沿用 RecalcUserLevel 的只升不降规则。可修改反向映射，但值必须是迁移 mapping 中映射到该新档的旧 key。报告给出总人数和前 50 个用户的 ID、当前组、目标旧组，不输出用户名/邮箱。
- 当前自定义分组不动；充值额、余额和其他资料不回写，已删除用户不重建。仅比较当前 group，无法识别中间改过又改回相同值的历史。
- abilities 只比较 group/model/channel_id 的结构与 abilities_after 是否相同；结构不同时中止，报前 20 个渠道 ID。恢复旧行时 enabled/priority 取当前对应新行的值（旧 key 映射、模型、渠道相同），允许自动启停和优先级变更，报告与原快照值不同的旧行数量。继承新增的新档行全部删除，自定义分组的独立行保留。
- 回滚先报告当前四档完整配置和当前 GroupRatio，再恢复旧配置。后台改折扣、返现、门槛或说明不会阻止回滚。当前自定义倍率保留，新四档 key 去掉，旧 key 恢复快照值；原先没有 GroupRatio 行时，只保留当前自定义 key，一个 key 都没有就不保留该行。自定义 key 已被删除的，不从旧快照重新加入。
- 新四档配置不全、旧 key 已存在或原快照 ID 被其他分组占用时中止。受影响渠道的 group 变化，或出现不在快照却包含新 key 的渠道时也中止，报前 20 个渠道 ID。确认迁移没有问题之前不要新增、删除或编辑渠道。
- 回滚提交后同样清理 Redis `user_group:*` 并重启全部实例。若快照原先没有 GroupRatio 行，恢复前应按第 1 节核对有效旧倍率，并按需补齐旧 key（包括只剩自定义 key 的情况）；正常生产流程已在发布前补齐旧倍率。
- 回滚脚本重复执行提示已回滚，无变更。回滚后若需要再次迁移，固定快照名会阻止覆盖：由用户归档原快照并审核另一套日期/批次前缀的脚本，不要自行删除快照。

## 6. 本地验证边界

`scripts/verify-tier-rename.pg.cjs` 使用已安装的 PGlite 和虚构数据，验证演练含序列/快照目录都无变化、提交结果、幂等、保护性中止和完整回滚。它不能模拟生产规模的锁等待、真实多实例并发或 Redis 清理。上线前仍需生产副本演练和用户审核。

第一轮验证曾通过 22 个场景。第二轮最终 40 个场景全部通过，虚构批量渠道用例只输出 29 条迁移 NOTICE（上限 100），包含继承、汇总输出上限、恢复流量后的回滚、结构冲突和本手册全部 SQL。Go build/vet 及 model/controller 测试通过，沿用 `GOFLAGS=-buildvcs=false` 和指定临时目录。只读节点中止分支用条件替换测试，实际只读节点路由、代理事务行为及真实多实例并发没有在本地验证。详细命令见计划与 CHANGELOG 的第二轮记录。

GORM 待核实项已对照本地 1.25.7 和 PostgreSQL 驱动 1.5.7 源码：创建回调在默认值填充前运行，字符串 tag 默认会显式写入零值字段；NewDB 复用 ConnPool；默认值变化可能进入 AlterColumn，类型不一致时可能附带类型变更。钩子查询使用原事务及保存点，避免查询错误让 PostgreSQL 创建事务失效。没有连接 PostgreSQL 实例验证 AutoMigrate DDL。

### 线上演练记录（2026-11-12）

线上库是带读写分离代理的集群地址，没有独立副本。经用户同意，直接在线上库演练。

- **只读检查**：五张表上没有触发器、外键、视图。除了脚本处理的五处，只有历史日志里出现旧等级名。渠道和 abilities 一致，同一渠道同一模型在各分组之间的 `enabled`、`priority` 没有不一致。
- **执行方式**：本机没有 psql，用 node 的 pg 驱动把整份文件作为一次请求发送。先用一个空事务确认了三件事：各语句在同一个事务里、在主节点上执行；NOTICE 能传回；事务事后的状态是已回滚。每次执行前静态检查文件的第一条语句是 `BEGIN`、最后一条是 `ROLLBACK`、中间没有任何能提交事务的语句。演练时把语句超时临时降到 30 秒，文件本身不改。
- **本地重放**：把线上导出的数据（只含分组相关的列）装进 PGlite，表结构和线上一致，完整跑了演练、迁移后立刻回滚、提交、重复执行、提交回滚，结果都符合预期。
- **演练一，只跑迁移**：约 3 秒，脚本内的断言全部通过，报告 29 条 NOTICE。演练前后五处数据的指纹、`group_configs` 的序列值、表的数量完全一致，没有留下快照表。
- **演练二，同一个事务里迁移后立刻回滚**：约 5 秒。回滚后五处数据的指纹和开始时完全一致。
- **仍未验证**：真实提交之后的 Redis 清理和实例重启；多实例并发下 `RecalcUserLevel` 的读写窗口；GORM AutoMigrate 在 PostgreSQL 实例上的 DDL。
