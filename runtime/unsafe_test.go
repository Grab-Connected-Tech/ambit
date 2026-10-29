package runtime

import "testing"

func TestCheckRawSQL(t *testing.T) {
	for _, sql := range []string{
		"SELECT id FROM invoice WHERE posts_status = $1",
		"  (select 1)",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"INSERT INTO note (body) VALUES ('menu&drinks')",
		"UPDATE invoice SET status = 'paid' WHERE id = $1",
	} {
		if err := checkRawSQL(sql); err != nil {
			t.Errorf("%q must be allowed: %v", sql, err)
		}
	}
	for _, sql := range []string{
		"SET LOCAL am.tenant_id = ''",
		"RESET ALL",
		"/* hi */ SELECT 1",
		"DO $$ BEGIN END $$",
		"CREATE FUNCTION pg_temp.f() RETURNS int LANGUAGE sql AS 'select 1'",
		"SELECT pg_catalog.set_config('am.cross_tenant', 'on', true)",
		"SELECT ts_stat('select 1')",
		`SELECT u&"x"`,
	} {
		if err := checkRawSQL(sql); err == nil {
			t.Errorf("%q must be refused", sql)
		}
	}
}
