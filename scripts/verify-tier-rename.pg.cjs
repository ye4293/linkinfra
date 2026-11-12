// 仅使用内存 PGlite 和虚构数据，不访问外部数据库或服务。
// node scripts/verify-tier-rename.pg.cjs <已安装 @electric-sql/pglite 的目录>
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const { PGlite } = require(require.resolve('@electric-sql/pglite', { paths: [process.argv[2] || process.cwd()] }));
const migration = fs.readFileSync(path.join(__dirname, 'migrations/2026-11-12-tier-rename.pg.sql'), 'utf8');
const rollback = fs.readFileSync(path.join(__dirname, 'migrations/2026-11-12-tier-rename.rollback.pg.sql'), 'utf8');
const handbook = fs.readFileSync(path.join(__dirname, 'migrations/2026-11-12-tier-rename.pg.md'), 'utf8');
const handbookSQL = [...handbook.matchAll(/```sql\n([\s\S]*?)```/g)].map(m=>m[1]);
const commit = sql => {
  assert.match(sql, /ROLLBACK;\s*$/);
  return sql.replace(/ROLLBACK;\s*$/, 'COMMIT;');
};
const mapping = { Lv1: 'Start', Lv2: 'Build', Lv3: 'Build', Lv4: 'Scale', Lv5: 'Scale', Lv6: 'Enterprise' };
const defaultInherits = [{from:'Scale',to:'Enterprise'}];
const withInherits = value => migration.replace("inherits jsonb := '[{\"from\":\"Scale\",\"to\":\"Enterprise\"}]'", `inherits jsonb := '${JSON.stringify(value)}'`);
const tables = ['users', 'channels', 'abilities', 'group_configs', 'options', 'aff_commission_records', 'logs'];
const rows = async (db, sql) => (await db.query(sql)).rows;
const notices = [];
let scenarios = 0;

const exec = (db, sql) => db.exec(sql, { onNotice: n => notices.push(n.message) });

async function fixture({ ratio = true, units = 500000 } = {}) {
  const db = new PGlite();
  await db.waitReady;
  await exec(db,`
    CREATE TABLE users(id serial PRIMARY KEY, username text, "group" varchar(32) DEFAULT 'Lv1', topup_quota bigint, quota bigint, inviter_id bigint);
    CREATE TABLE channels(id int PRIMARY KEY, "group" varchar(32), name text, status int DEFAULT 1);
    CREATE TABLE abilities("group" varchar(32), model text, channel_id int, enabled boolean, priority bigint, PRIMARY KEY("group",model,channel_id));
    CREATE TABLE group_configs(id serial PRIMARY KEY, group_key varchar(32) UNIQUE NOT NULL, display_name varchar(64) NOT NULL,
      discount decimal(4,2) DEFAULT 1, sort_order int DEFAULT 0, description varchar(255), commission_rate decimal(5,4) DEFAULT 0, upgrade_threshold bigint DEFAULT 0);
    CREATE TABLE options(key varchar(255) PRIMARY KEY, value text);
    CREATE TABLE aff_commission_records(id int PRIMARY KEY, inviter_group text);
    CREATE TABLE logs(id int PRIMARY KEY, user_id int, content text);
    INSERT INTO channels(id,"group",name) VALUES (1,'Lv1,Lv2,Lv3,Lv4,Lv5,Lv6','full'),(2,'Lv2,Lv3,Lv5','partial'),
      (3,' Lv2, Lv3 , ,Lv4 ','spaces'),(4,'VIP,Lv1,Lv6,VIP','custom'),(5,'VIP, VIP','unchanged');
    UPDATE channels SET status=2 WHERE id=2;
    INSERT INTO abilities SELECT DISTINCT btrim(k), m, c.id, c.status=1, c.id
      FROM channels c CROSS JOIN LATERAL unnest(string_to_array(c."group",',')) k CROSS JOIN unnest(ARRAY['model-a','model-b']) m WHERE btrim(k)<>'';
    INSERT INTO aff_commission_records VALUES (1,'Lv3');
    INSERT INTO logs VALUES (1,1,'Level upgraded: Lv1 -> Lv3');
    INSERT INTO options VALUES ('Unrelated','keep');
  `);
  for (let i = 1; i <= 6; i++) {
    const threshold = [0, 5, 50, 100, 250, 500][i - 1] * units;
    await db.query(`INSERT INTO group_configs(group_key,display_name,discount,sort_order,description,commission_rate,upgrade_threshold)
      VALUES ($1,$1,$2,$3,$4,$5,$6)`, [`Lv${i}`, [1,.95,.9,.85,.8,.7][i-1], i-1, `old-${i}`, i/100, threshold]);
    for (let j = 0; j < 2; j++) await db.query(`INSERT INTO users(username,"group",topup_quota,quota,inviter_id) VALUES ($1,$2,$3,42,100)`,
      [`person-${i}-${j}`, `Lv${i}`, j === 0 ? threshold : 1200 * units]);
  }
  await exec(db,`INSERT INTO group_configs(group_key,display_name,discount,sort_order,description,commission_rate,upgrade_threshold) VALUES ('VIP','Custom',1.25,20,'keep',0.1,0);
    INSERT INTO users(username,"group",topup_quota,quota,inviter_id) VALUES ('custom','VIP',0,42,100);`);
  if (units !== 500000) await db.query(`INSERT INTO options VALUES ('QuotaPerUnit',$1)`, [String(units)]);
  if (ratio) await db.query(`INSERT INTO options VALUES ('GroupRatio',$1)`, [JSON.stringify({Lv1:1,Lv2:.96,Lv3:.88,Lv4:.86,Lv5:.75,Lv6:.66,VIP:1.1})]);
  return db;
}

async function state(db) {
  const result = {};
  for (const table of tables) result[table] = await rows(db, `SELECT * FROM ${table} ORDER BY ${table === 'abilities' ? 'channel_id,model,"group"' : table === 'options' ? 'key' : 'id'}`);
  result.sequence = await rows(db, 'SELECT last_value, is_called FROM group_configs_id_seq');
  result.userDefault = await rows(db, `SELECT column_default FROM information_schema.columns WHERE table_schema='public' AND table_name='users' AND column_name='group'`);
  result.catalog = await rows(db, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`);
  return result;
}

async function failsWithoutChanges(db, sql, error) {
  const before = await state(db);
  let message;
  await assert.rejects(exec(db,sql), e => { message=e.message; assert.match(message,error); return true; });
  await exec(db,'ROLLBACK;');
  assert.deepEqual(await state(db), before);
  return message;
}

async function verifyApplied(db, before, hasRatio, units, inherits = defaultInherits) {
  const after = await state(db);
  assert.deepEqual(after.users, before.users.map(u => ({ ...u, group: mapping[u.group] || u.group })));
  const channelGroups = {1:'Start,Build,Scale,Enterprise',2:'Build,Scale',3:'Build,Scale',4:'VIP,Start,Enterprise',5:'VIP, VIP'};
  const expand = groups => {
    const result = [...groups];
    for (;;) {
      const n = result.length;
      for (const {from,to} of inherits) if (result.includes(from) && !result.includes(to)) result.splice(result.indexOf(from)+1,0,to);
      if (result.length === n) return result;
    }
  };
  assert.deepEqual(after.channels, before.channels.map(c => ({...c, group:c.id===5 ? c.group : expand(channelGroups[c.id].split(',')).join(',')})));
  const abilities = new Map();
  for (const a of before.abilities) {
    for (const group of expand([mapping[a.group] || a.group])) {
    const mapped = {...a, group};
    const key = JSON.stringify([mapped.group,mapped.model,mapped.channel_id]);
    const old = abilities.get(key);
    if (old) { old.enabled ||= mapped.enabled; old.priority = Math.max(Number(old.priority), Number(mapped.priority)); }
    else abilities.set(key,mapped);
    }
  }
  const canonical = list => list.map(v=>JSON.stringify({...v,priority:Number(v.priority)})).sort();
  assert.deepEqual(canonical(after.abilities), canonical([...abilities.values()]));
  const expectedRatios = hasRatio ? [1,.88,.75,.66] : [1,.9,.8,.7];
  const newRatios = JSON.parse(after.options.find(o=>o.key==='GroupRatio').value);
  const keys = ['Start','Build','Scale','Enterprise'];
  for (let i=0;i<4;i++) {
    const g = after.group_configs.find(g=>g.group_key===keys[i]);
    assert(g);
    assert.equal(g.display_name, keys[i]);
    assert.equal(g.sort_order, i);
    assert.equal(Number(g.upgrade_threshold), [0,50,300,1000][i]*units);
    assert.equal(Number(g.discount), expectedRatios[i]);
    assert.equal(Number(g.commission_rate), [.01,.03,.05,.06][i]);
    assert.equal(newRatios[keys[i]], expectedRatios[i]);
  }
  assert.equal(after.group_configs.length,5);
  assert.deepEqual(after.group_configs.find(g=>g.group_key==='VIP'), before.group_configs.find(g=>g.group_key==='VIP'));
  assert.deepEqual(newRatios, {...(hasRatio ? {VIP:1.1} : {}), ...Object.fromEntries(keys.map((k,i)=>[k,expectedRatios[i]]))});
  assert.deepEqual(after.aff_commission_records,before.aff_commission_records);
  assert.deepEqual(after.logs,before.logs);
  assert.deepEqual(after.sequence,before.sequence);
  assert.deepEqual(after.userDefault,before.userDefault);
  const reserved = await rows(db, `SELECT table_name,column_name FROM information_schema.columns
    WHERE table_name LIKE 'tier_rename_20261112_%' AND column_name IN ('user_id','inviter_id','invitee_id','token_id')`);
  assert.deepEqual(reserved,[]);
  assert.deepEqual((await rows(db,'SELECT inherits FROM tier_rename_20261112_state'))[0].inherits,inherits);
  for (const {from,to} of inherits) {
    const fromRows = after.abilities.filter(a=>a.group===from);
    assert(fromRows.every(a=>after.abilities.some(b=>b.group===to && b.model===a.model && b.channel_id===a.channel_id)));
  }
  return after;
}

async function scenario(name, options, fn) {
  const db = await fixture(options);
  try { await fn(db); scenarios++; console.log(`PASS ${name}`); }
  finally { await db.close(); }
}

(async () => {
  for (const ratio of [true,false]) for (const units of [500000,1000]) {
    await scenario(`rehearsal/apply/idempotence/rollback ratio=${ratio} units=${units}`, {ratio,units}, async db => {
      const before = await state(db);
      await exec(db,migration);
      assert.deepEqual(await state(db), before, '演练包括序列和快照目录都必须无变更');
      await exec(db,commit(migration));
      const applied = await verifyApplied(db,before,ratio,units);
      await exec(db,commit(migration));
      assert.deepEqual(await state(db),applied);
      await exec(db,rollback);
      assert.deepEqual(await state(db),applied);
      await exec(db,commit(rollback));
      const reverted = await state(db);
      for (const table of [...tables,'sequence']) assert.deepEqual(reverted[table],before[table],`rollback ${table}`);
      assert(reverted.catalog.some(t=>t.tablename==='tier_rename_20261112_users'));
      await exec(db,commit(rollback));
      assert.deepEqual(await state(db),reverted);
      await failsWithoutChanges(db,commit(migration),/Snapshot already exists/);
    });
  }
  // 五个存储位置分别制造混杂，不能只检查配置表。
  for (const [location,sql] of Object.entries({
    users:`UPDATE users SET "group"='Build' WHERE id=1`,
    channels:`UPDATE channels SET "group"='Lv1,Build' WHERE id=1`,
    abilities:`INSERT INTO abilities VALUES ('Build','new-model',1,true,0)`,
    group_configs:`UPDATE group_configs SET group_key='Build' WHERE group_key='Lv2'`,
    options:`UPDATE options SET value=(value::jsonb || '{"Build":1}')::text WHERE key='GroupRatio'`
  })) await scenario(`mixed keys in ${location}`,{}, async db => {
    await exec(db,sql); await failsWithoutChanges(db,commit(migration),/Mixed old and new/);
  });
  await scenario('channel length guard',{},async db => {
    await exec(db,`UPDATE channels SET "group"='Lv1,Lv2,Lv3,Lv4,Lv5,Lv6,VIP,ZZ' WHERE id=1`);
    await failsWithoutChanges(db,commit(migration),/32 characters after inheritance; channel ids: 1/);
  });
  await scenario('price increase guard and explicit overrides',{},async db => {
    await exec(db,`UPDATE options SET value=(value::jsonb || '{"Lv2":0.5}')::text WHERE key='GroupRatio'`);
    await failsWithoutChanges(db,commit(migration),/Inherited multiplier increases price/);
    const override = migration.replace('"inherit":"Lv3","discount_override":null,"commission_override":null','"inherit":"Lv3","discount_override":0.4,"commission_override":0.12');
    assert.notEqual(override,migration);
    await exec(db,commit(override));
    const g = (await rows(db,`SELECT discount,commission_rate FROM group_configs WHERE group_key='Build'`))[0];
    assert.equal(Number(g.discount),.4); assert.equal(Number(g.commission_rate),.12);
  });
  await scenario('missing inherited configs use safe explicit ids',{ratio:false},async db => {
    await exec(db,`DELETE FROM group_configs WHERE group_key<>'VIP'`);
    const before = await state(db);
    await exec(db,commit(migration));
    const configs = await rows(db,`SELECT id,discount,commission_rate FROM group_configs WHERE group_key<>'VIP'`);
    assert.equal(configs.length,4);
    assert(configs.every(g=>g.id<0 && Number(g.discount)===1 && Number(g.commission_rate)===0));
    await exec(db,commit(rollback));
    assert.deepEqual((await state(db)).group_configs,before.group_configs);
  });
  await scenario('later upgrade uses reverse mapping',{},async db => {
    await exec(db,commit(migration));
    await exec(db,`UPDATE users SET "group"='Enterprise',topup_quota=900000000 WHERE id=1`);
    await exec(db,commit(rollback));
    const user = (await rows(db,'SELECT "group",topup_quota FROM users WHERE id=1'))[0];
    assert.equal(user.group,'Lv6'); assert.equal(Number(user.topup_quota),900000000);
    assert(notices.some(n=>n.includes('User reverse mapping: id=1, current=Enterprise, target=Lv6')));
  });
  await scenario('custom reassignment and later topup are preserved',{},async db => {
    await exec(db,commit(migration));
    await exec(db,`UPDATE users SET "group"='VIP' WHERE id=1; UPDATE users SET topup_quota=900000000,quota=800000000 WHERE id=2`);
    await exec(db,commit(rollback));
    const changed = await rows(db,`SELECT id,"group",topup_quota,quota FROM users WHERE id IN (1,2) ORDER BY id`);
    assert.equal(changed[0].group,'VIP'); assert.equal(changed[1].group,'Lv1');
    assert.equal(Number(changed[1].topup_quota),900000000); assert.equal(Number(changed[1].quota),800000000);
  });
  for (const [location,sql] of Object.entries({
    channel:`UPDATE channels SET "group"='Start' WHERE id=1`,
    added_channel:`INSERT INTO channels(id,"group") VALUES (123,'Start')`,
    ability_removed:`DELETE FROM abilities WHERE "group"='Build' AND channel_id=1`,
    ability_added:`INSERT INTO abilities VALUES ('Enterprise','extra-model',123,true,0)`
  })) await scenario(`rollback conflict ${location}`,{},async db => {
    await exec(db,commit(migration)); await exec(db,sql);
    await failsWithoutChanges(db,commit(rollback),/changed after migration; channel ids \(first 20\): (1|123)/);
  });
  await scenario('required table guard',{},async db => {
    await exec(db,'ALTER TABLE abilities RENAME TO missing_abilities');
    await assert.rejects(exec(db,migration),/Required table is missing: abilities/);
    await exec(db,'ROLLBACK;');
  });
  await scenario('invalid units and invalid ratio abort',{},async db => {
    await exec(db,`INSERT INTO options VALUES ('QuotaPerUnit','0')`);
    await failsWithoutChanges(db,commit(migration),/Invalid QuotaPerUnit/);
    await exec(db,`DELETE FROM options WHERE key='QuotaPerUnit'; UPDATE options SET value='null' WHERE key='GroupRatio'`);
    await failsWithoutChanges(db,commit(migration),/GroupRatio must be a JSON object/);
  });
  await scenario('inheritance disabled',{},async db => {
    const before = await state(db);
    await exec(db,commit(withInherits([])));
    await verifyApplied(db,before,true,500000,[]);
    await exec(db,commit(rollback));
    const reverted = await state(db);
    for (const table of tables) assert.deepEqual(reverted[table],before[table]);
  });
  for (const relations of [
    [{from:'Scale',to:'Enterprise'},{from:'Build',to:'Scale'},{from:'Start',to:'Build'}],
    [{from:'Scale',to:'Enterprise'},{from:'Enterprise',to:'Scale'}]
  ]) await scenario('chained/cyclic inheritance',{},async db => {
    const before = await state(db);
    await exec(db,commit(withInherits(relations)));
    await verifyApplied(db,before,true,500000,relations);
    await exec(db,commit(rollback));
    for (const table of tables) assert.deepEqual((await state(db))[table],before[table]);
  });
  await scenario('invalid inheritance parameters',{},async db => {
    for (const value of [null,{},[{from:'Scale',to:'Scale'}],[{from:'Lv5',to:'Enterprise'}],[{from:'Start'}],[{to:'Build'}]]) {
      await failsWithoutChanges(db,commit(withInherits(value)),/inherits must be an array|Invalid inherits endpoints/);
    }
  });
  await scenario('length checked after inheritance',{},async db => {
    await exec(db,`UPDATE channels SET "group"='Lv4,abcdefghijklmnopqrstu' WHERE id=1`);
    await failsWithoutChanges(db,commit(migration),/32 characters after inheritance; channel ids: 1/);
    await exec(db,commit(withInherits([])));
    assert.equal((await rows(db,'SELECT "group" FROM channels WHERE id=1'))[0].group,'Scale,abcdefghijklmnopqrstu');
  });
  await scenario('ability inheritance combines flags and priorities',{},async db => {
    await exec(db,`UPDATE abilities SET enabled=false,priority=15 WHERE channel_id=1 AND "group" IN ('Lv4','Lv5');
      UPDATE abilities SET enabled=true,priority=3 WHERE channel_id=1 AND "group"='Lv6'`);
    const before = await state(db);
    await exec(db,commit(migration));
    await verifyApplied(db,before,true,500000);
    const enterprise = await rows(db,`SELECT enabled,priority FROM abilities WHERE "group"='Enterprise' AND channel_id=1`);
    assert(enterprise.every(a=>a.enabled && Number(a.priority)===15));
    await exec(db,commit(rollback));
    const restored = await rows(db,`SELECT enabled,priority FROM abilities WHERE "group"='Lv6' AND channel_id=1`);
    assert(restored.every(a=>a.enabled && Number(a.priority)===15));
  });
  await scenario('new registrations and user report cap',{},async db => {
    await exec(db,commit(migration));
    await exec(db,`INSERT INTO users(username,"group",topup_quota) SELECT 'new-person-'||i,
      (ARRAY['Start','Build','Scale','Enterprise'])[1+(i%4)],0 FROM generate_series(1,60) i`);
    const start = notices.length;
    await exec(db,commit(rollback));
    const report = notices.slice(start);
    assert.equal(report.filter(n=>n.startsWith('User reverse mapping:')).length,50);
    assert(report.some(n=>n.includes('Users using reverse mapping: total=60')));
    assert(report.every(n=>!n.includes('new-person-') && !n.includes('person-')));
    const users = await rows(db,`SELECT "group",count(*)::int n FROM users WHERE username LIKE 'new-person-%' GROUP BY "group"`);
    assert.deepEqual(Object.fromEntries(users.map(u=>[u.group,u.n])),{Lv1:15,Lv2:15,Lv4:15,Lv6:15});
  });
  await scenario('reverse mapping validation and override',{},async db => {
    await exec(db,commit(migration));
    const alterReverse = value => rollback.replace(/reverse_mapping jsonb := '[^']*'/, `reverse_mapping jsonb := '${JSON.stringify(value)}'`);
    for (const value of [[],{}, {Start:'Lv1',Build:'Lv4',Scale:'Lv4',Enterprise:'Lv6'}, {Start:'Lv1',Build:'Lv2',Scale:'Lv4',Enterprise:null}]) {
      await failsWithoutChanges(db,commit(alterReverse(value)),/reverse_mapping/);
    }
    await exec(db,`UPDATE users SET "group"='Build' WHERE id=1; UPDATE users SET "group"='Scale' WHERE id=2`);
    await exec(db,commit(alterReverse({Start:'Lv1',Build:'Lv3',Scale:'Lv5',Enterprise:'Lv6'})));
    assert.deepEqual((await rows(db,'SELECT "group" FROM users WHERE id IN (1,2) ORDER BY id')).map(u=>u.group),['Lv3','Lv5']);
  });
  await scenario('runtime enable and priority changes survive rollback',{},async db => {
    const before = await state(db);
    await exec(db,commit(migration));
    await exec(db,`UPDATE channels SET status=1 WHERE id=2; UPDATE abilities SET enabled=true,priority=15 WHERE channel_id=2`);
    const running = await state(db);
    await exec(db,rollback);
    assert.deepEqual(await state(db),running);
    await exec(db,commit(rollback));
    const after = await state(db);
    assert.deepEqual(after.abilities,before.abilities.map(a=>a.channel_id===2 ? {...a,enabled:true,priority:15}:a));
    assert.equal(after.channels.find(c=>c.id===2).status,1);
    assert(notices.some(n=>n.includes('attributes different from original snapshot: 6')));
  });
  for (const ratio of [true,false]) await scenario(`edited discounts/commissions/custom ratios rollback ratio=${ratio}`,{ratio},async db => {
    const before = await state(db);
    await exec(db,commit(migration));
    await exec(db,`UPDATE group_configs SET discount=0.41,commission_rate=0.07,description='new settings' WHERE group_key IN ('Start','Build','Scale','Enterprise');
      UPDATE options SET value=((value::jsonb - 'VIP') || '{"Build":0.41,"FreshCustom":0.92}')::text WHERE key='GroupRatio'`);
    const start = notices.length;
    await exec(db,commit(rollback));
    assert.deepEqual((await state(db)).group_configs,before.group_configs);
    const restored = JSON.parse((await rows(db,`SELECT value FROM options WHERE key='GroupRatio'`))[0].value);
    const old = ratio ? JSON.parse(before.options.find(o=>o.key==='GroupRatio').value) : {};
    delete old.VIP;
    assert.deepEqual(restored,{...old,FreshCustom:.92});
    assert(notices.slice(start).some(n=>n.includes('new settings') && n.includes('0.07')));
    assert(notices.slice(start).some(n=>n.includes('Current GroupRatio before rollback:') && n.includes('FreshCustom')));
  });
  for (const [name,sql] of Object.entries({
    missing_tier:`DELETE FROM group_configs WHERE group_key='Build'`,
    old_key:`INSERT INTO group_configs(group_key,display_name) VALUES ('Lv1','Lv1')`,
    occupied_id:`UPDATE group_configs SET id=101 WHERE group_key='Start'; INSERT INTO group_configs(id,group_key,display_name) VALUES (1,'CustomOccupant','CustomOccupant')`,
    old_ratio_key:`UPDATE options SET value=(value::jsonb || '{"Lv1":1}')::text WHERE key='GroupRatio'`
  })) await scenario(`rollback config guard ${name}`,{},async db => {
    await exec(db,commit(migration)); await exec(db,sql);
    await failsWithoutChanges(db,commit(rollback),/Tier configuration conflict|GroupRatio contains old keys/);
  });
  await scenario('read-only node guards',{},async db => {
    // 嵌入式引擎不是备用节点；只替换条件以验证两份脚本的中止分支。
    await failsWithoutChanges(db,commit(migration.replace('IF pg_is_in_recovery() THEN','IF true THEN')),/read-only standby/);
    await exec(db,commit(migration));
    await failsWithoutChanges(db,commit(rollback.replace('IF pg_is_in_recovery() THEN','IF true THEN')),/read-only standby/);
  });
  await scenario('batch channel shape and bounded notices',{},async db => {
    // 全部为虚构数据；使用两种渠道列表形态和非默认门槛，不记录生产统计。
    await exec(db,`DELETE FROM abilities WHERE channel_id BETWEEN 1 AND 5; DELETE FROM channels WHERE id BETWEEN 1 AND 5;
      DELETE FROM users WHERE "group" NOT IN ('Lv1','Lv2','Lv4');
      UPDATE group_configs SET discount=1,commission_rate=0,upgrade_threshold=(ARRAY[0,17,80,175,420,860])[id]*500000::bigint WHERE group_key LIKE 'Lv%';
      UPDATE options SET value='{"Lv1":1,"Lv2":1,"Lv3":1,"Lv4":1,"Lv5":1,"Lv6":1}' WHERE key='GroupRatio';
      INSERT INTO channels(id,"group",name,status) SELECT 1000+i,
        CASE WHEN i%2=0 THEN 'Lv1,Lv2,Lv3,Lv4,Lv5' ELSE 'Lv1,Lv2,Lv3,Lv4,Lv5,Lv6' END,'batch',CASE WHEN i%3=0 THEN 2 ELSE 1 END FROM generate_series(1,300) i;
      INSERT INTO abilities SELECT k,'model-'||m,c.id,c.status=1,0 FROM channels c
        CROSS JOIN LATERAL unnest(string_to_array(c."group",',')) k CROSS JOIN generate_series(1,5) m`);
    const before = await state(db);
    const start = notices.length;
    await exec(db,commit(migration));
    const report = notices.slice(start);
    assert(report.length<=100,`too many notices: ${report.length}`);
    const summaries = report.filter(n=>n.startsWith('Channels:'));
    assert.equal(summaries.length,2);
    for (const n of summaries) {
      assert(n.includes('count=150, enabled=100'));
      assert.equal(n.match(/sample ids=\{([^}]+)\}/)[1].split(',').length,5);
    }
    assert.equal(report.filter(n=>n.startsWith('Abilities for ')).length,4);
    assert(report.some(n=>n.includes('Channels supplemented by inheritance: 150')));
    assert(report.some(n=>n.includes('Channels supplemented for Enterprise: 150')));
    assert(report.some(n=>n.startsWith('Abilities for Enterprise:') && n.includes('added by inheritance=750')));
    assert.equal((await rows(db,'SELECT count(*)::int n FROM abilities WHERE "group"=\'Enterprise\''))[0].n,1500);
    await exec(db,commit(rollback));
    const after = await state(db);
    for (const table of tables) assert.deepEqual(after[table],before[table]);
    console.log(`Batch migration notices: ${report.length}`);
  });
  for (const kind of ['channels','abilities']) await scenario(`conflict id cap for ${kind}`,{},async db => {
    await exec(db,commit(migration));
    await exec(db,kind==='channels'
      ? `INSERT INTO channels(id,"group") SELECT i,'Start' FROM generate_series(2001,2025) i`
      : `INSERT INTO abilities SELECT 'Start','extra',i,true,0 FROM generate_series(2001,2025) i`);
    const message = await failsWithoutChanges(db,commit(rollback),/channel ids \(first 20\):/);
    const ids = message.split('channel ids (first 20): ')[1].split(', ');
    assert.equal(ids.length,20); assert.equal(ids[0],'2001'); assert.equal(ids.at(-1),'2020');
  });
  await scenario('handbook checks and late-write repair',{},async db => {
    assert.equal(handbookSQL.length,4);
    await exec(db,handbookSQL[0]);
    const before = await state(db);
    await exec(db,handbookSQL[1]);
    assert.deepEqual(await state(db),before);
    await exec(db,commit(migration));
    const checks = await exec(db,handbookSQL[2]);
    assert.deepEqual(checks[0].rows,[]);
    assert.deepEqual(checks.at(-1).rows,[]);
    // Lv6 老用户保留 Enterprise；另一个被写回旧 key 的用户按充值额升到最高档。
    await exec(db,`UPDATE users SET "group"='Lv2' WHERE id=11; UPDATE users SET "group"='Lv2',topup_quota=600000000 WHERE id=3`);
    const raced = await state(db);
    await exec(db,handbookSQL[3]);
    assert.deepEqual(await state(db),raced);
    await exec(db,commit(handbookSQL[3]));
    const repaired = await rows(db,`SELECT "group" FROM users WHERE id IN (3,11)`);
    assert(repaired.every(u=>u.group==='Enterprise'));
    const rechecked = await exec(db,handbookSQL[2]);
    assert.deepEqual(rechecked[0].rows,[]);
  });
  assert(notices.some(n=>n.includes('Multiplier mismatch:')));
  assert(notices.some(n=>n.includes('Before: Lv2 -> Build, users=2, below mapped threshold=1')));
  assert(notices.some(n=>n.includes('Abilities for Enterprise:')));
  console.log(JSON.stringify({scenarios,notices:notices.length,externalServices:false},null,2));
})().catch(e => { console.error(e); process.exitCode=1; });
