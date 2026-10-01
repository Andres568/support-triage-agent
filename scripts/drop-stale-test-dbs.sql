-- Drop throwaway databases left behind by crashed `make db-check` runs.
-- Names carry their creation time: triage_{check,tmpl,t}_<unix seconds>_<random>.
-- Only databases older than an hour are dropped, so a concurrent run's
-- databases are never touched. The unsuffixed names are from before the
-- naming scheme existed and are always stale.
-- Usage: psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/drop-stale-test-dbs.sql
SELECT format('DROP DATABASE IF EXISTS %I WITH (FORCE)', datname)
FROM pg_database
WHERE (datname ~ '^triage_(check|tmpl|t|eval)_[0-9]{10}_'
       -- CASE, not AND: Postgres may evaluate the cast before the regex.
       AND CASE WHEN split_part(datname, '_', 3) ~ '^[0-9]{10}$'
                 THEN split_part(datname, '_', 3)::bigint < extract(epoch FROM now()) - 3600 END)
   OR datname IN ('triage_check', 'triage_tmpl')
   OR (datname LIKE 'triage\_t\_%' AND datname !~ '^triage_t_[0-9]{10}_')
ORDER BY datname
\gexec
