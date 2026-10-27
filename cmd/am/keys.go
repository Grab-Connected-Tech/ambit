package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Grab-Connected-Tech/ambit/runtime"
)

// cmdKeys manages the keys of Encrypted() fields (§9.4):
//
//	am keys new      print a fresh key for AM_ENCRYPTION_KEY
//	am keys rotate   re-seal every value under the primary key
//
// To rotate: set AM_ENCRYPTION_KEY to the new key, move the old one into
// AM_ENCRYPTION_KEYS_OLD, deploy, run am keys rotate, then drop the old key.
func cmdKeys(args []string) error {
	fs, asJSON, err := flags("keys", args, nil)
	if err != nil {
		return err
	}
	switch fs.Arg(0) {
	case "new":
		fmt.Println(runtime.NewKey())
		return nil
	case "rotate":
		url, err := databaseURL("AM_DATABASE_URL")
		if err != nil {
			return err
		}
		n, err := rotateKeys(context.Background(), url)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(map[string]int{"resealed": n})
		} else {
			fmt.Printf("re-sealed %d values\n", n)
		}
		return nil
	}
	return fmt.Errorf("usage: am keys new|rotate")
}

func rotateKeys(ctx context.Context, url string) (int, error) {
	app, diags, err := loadModel()
	if err != nil {
		return 0, err
	}
	if hasErrors(diags) {
		return 0, report(diags, false, false)
	}
	keys, err := runtime.KeyringFromConfig(runtime.Defaults())
	if err != nil {
		return 0, err
	}
	if keys == nil {
		return 0, fmt.Errorf("set AM_ENCRYPTION_KEY to the new key and AM_ENCRYPTION_KEYS_OLD to the old ones")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT set_config('am.cross_tenant', 'on', false)"); err != nil {
		return 0, err
	}
	total := 0
	for _, r := range app.AllResources() {
		for _, f := range r.Resource.Fields {
			if !f.Type.Encrypted {
				continue
			}
			table, col := r.Resource.Table, f.Column
			// Keyset batches, so a large table never holds one long transaction.
			cursor := "00000000-0000-0000-0000-000000000000"
			for {
				rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT id::text, %s FROM %s WHERE id > $1::uuid AND %s IS NOT NULL ORDER BY id LIMIT 500`,
					quoteIdent(col), quoteIdent(table), quoteIdent(col)), cursor)
				if err != nil {
					return total, err
				}
				type item struct {
					id     string
					sealed []byte
				}
				var batch []item
				for rows.Next() {
					var it item
					if err := rows.Scan(&it.id, &it.sealed); err != nil {
						rows.Close()
						return total, err
					}
					batch = append(batch, it)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return total, err
				}
				if len(batch) == 0 {
					break
				}
				for _, it := range batch {
					cursor = it.id
					if kid, _ := runtime.KeyOf(it.sealed); kid == keys.Primary() {
						continue
					}
					plain, err := keys.Open(it.sealed, table, col, it.id)
					if err != nil {
						return total, fmt.Errorf("%s.%s row %s: %w", table, col, it.id, err)
					}
					resealed, err := keys.Seal(plain, table, col, it.id)
					if err != nil {
						return total, err
					}
					if _, err := conn.Exec(ctx, fmt.Sprintf(`UPDATE %s SET %s = $1 WHERE id = $2::uuid`, quoteIdent(table), quoteIdent(col)), resealed, it.id); err != nil {
						return total, err
					}
					total++
				}
			}
		}
	}
	return total, nil
}
