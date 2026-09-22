// 在隔离PGlite(PostgreSQL引擎)执行SQL/计划验证，不访问生产库。
// node scripts/verify-metrics-v2.pg.cjs <安装@electric-sql/pglite的目录>
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const { PGlite } = require(require.resolve('@electric-sql/pglite', { paths: [process.argv[2] || process.cwd()] }));
(async () => {
 const db = new PGlite(); await db.waitReady;
 await db.exec(`CREATE TABLE logs(id bigserial PRIMARY KEY, created_at bigint, type int, model_name varchar(200), other text);
 CREATE INDEX idx_logs_created_at ON logs(created_at);`);
 // 嵌入式引擎不模拟并发建索引；生产脚本仍保留CONCURRENTLY。
 const migration = fs.readFileSync(path.join(__dirname,'migrations/2026-09-22-metrics-v2.pg.sql'),'utf8').replace(/CREATE INDEX CONCURRENTLY/g,'CREATE INDEX');
 await db.exec(migration);
 await db.exec(`INSERT INTO logs(created_at,type,model_name,other,metrics_version,metrics_source_key,metrics_attempts)
 SELECT 1789344000+i/10, CASE WHEN i%10=0 THEN 5 ELSE 2 END, 'gpt-'||(i%100), repeat('metadata',80),
 CASE WHEN i%5=0 THEN 0 ELSE 2 END,'azure','[{"source":"azure","channel":1,"outcome":"success","duration":2}]'
 FROM generate_series(1,1000000) i;`);
 await db.exec('ANALYZE logs');
 await db.exec(`SET plan_cache_mode=force_generic_plan;
 PREPARE metrics_page(bigint,bigint,bigint,bigint) AS
 SELECT id,created_at,model_name,metrics_attempts FROM logs
 WHERE type IN (2,5) AND metrics_version=2 AND created_at >= $1 AND created_at < $2
 AND (created_at,id)>($3,$4) ORDER BY created_at,id LIMIT 100;`);
 const plan=(await db.query('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE metrics_page(1789344300,1789344600,1789344300,0)')).rows[0]['QUERY PLAN'];
 const nodes=n=>[n,...(n.Plans||[]).flatMap(nodes)];
 assert(nodes(plan[0].Plan).some(n=>['idx_logs_metrics_v2_scan','idx_logs_created_at'].includes(n['Index Name'])),'metrics scan must use a bounded time index');
 assert(!nodes(plan[0].Plan).some(n=>n['Node Type']==='Seq Scan'),'metrics page must not scan whole table');
 const rows=(await db.query('EXECUTE metrics_page(1789344300,1789344600,1789344300,0)')).rows;
 assert.equal(rows.length,100);
 // 少量V2数据与同秒热点，验证部分索引确实可用，不能只检查DDL存在。
 await db.exec(`UPDATE logs SET metrics_version=CASE WHEN id%100=1 THEN 2 ELSE 0 END;
 ANALYZE logs; DEALLOCATE metrics_page; SET plan_cache_mode=force_generic_plan;
 PREPARE metrics_page(bigint,bigint,bigint,bigint) AS SELECT id,created_at,model_name,metrics_attempts FROM logs
 WHERE type IN (2,5) AND metrics_version=2 AND created_at >= $1 AND created_at < $2
 AND (created_at,id)>($3,$4) ORDER BY created_at,id LIMIT 100;`);
 const sparsePlan=(await db.query('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE metrics_page(1789344300,1789344600,1789344300,0)')).rows[0]['QUERY PLAN'];
 assert(nodes(sparsePlan[0].Plan).some(n=>n['Index Name']==='idx_logs_metrics_v2_scan'),'sparse V2 rows must use partial index');
 await db.exec(`INSERT INTO metrics_v2_state VALUES(1,0,0,0,'',0,0,0,false);
 UPDATE metrics_v2_state SET owner='worker-a',lease_until=EXTRACT(EPOCH FROM clock_timestamp())::bigint+60,generation=generation+1 WHERE id=1 AND lease_until<=EXTRACT(EPOCH FROM clock_timestamp())::bigint;`);
 const lease=await db.query(`UPDATE metrics_v2_state SET owner='worker-b' WHERE id=1 AND lease_until<=EXTRACT(EPOCH FROM clock_timestamp())::bigint RETURNING id`);
 assert.equal(lease.rows.length,0);
 const valid=(await db.query(`SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='logs'::regclass AND c.relname='idx_logs_metrics_v2_scan' AND i.indisvalid AND i.indisready AND pg_get_indexdef(i.indexrelid) LIKE '%(created_at, id)%' AND pg_get_expr(i.indpred,i.indrelid) LIKE '%metrics_version = 2%') AS valid`)).rows[0].valid;
 assert(valid);
 console.log(JSON.stringify({rows:1000000,genericPlan:plan[0],sparsePlan:sparsePlan[0],leaseExclusion:true,indexValid:true},null,2));
 await db.close();
})().catch(e=>{console.error(e);process.exit(1)});
