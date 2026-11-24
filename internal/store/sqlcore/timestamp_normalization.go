// SPDX-License-Identifier: Apache-2.0

package sqlcore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// NormalizeTimestamps upgrades UTC strings written by older controllers before
// this leader dispatches work. Leave optional NULL and empty values intact.
func (c *Core) NormalizeTimestamps(ctx context.Context) (int64, error) {
	var rewritten int64
	for _, table := range []struct {
		name, keys string
		columns    []string
	}{
		{"actions", "id", []string{"created_at", "updated_at"}},
		{"event_outbox", "id", []string{"created_at", "updated_at"}},
		{"events", "id", []string{"timestamp"}},
		{"incidents", "id", []string{"opened_at", "updated_at", "resolved_at", "state_changed_at"}},
		{"operational_resources", "kind,id", []string{"created_at", "expires_at"}},
		{"operational_idempotency", "kind,actor,idempotency_key", []string{"expires_at"}},
	} {
		for _, col := range table.columns {
			// All identifiers are fixed above. Legacy UTC output has a terminal Z
			// and either no fraction or one to eight fractional digits.
			// SQL substr with a negative start differs between the engines.
			predicate := fmt.Sprintf("((length(%[1]s)=20 OR length(%[1]s) BETWEEN 22 AND 29) AND substr(%[1]s,length(%[1]s),1)='Z')", col)
			n, err := c.normalizeColumn(ctx, table.name, table.keys, col, predicate)
			rewritten += n
			if err != nil {
				return rewritten, fmt.Errorf("normalize %s.%s: %w", table.name, col, err)
			}
		}
	}
	return rewritten, nil
}

// normalizeColumn advances over the primary key, committing at most 512 legacy
// values per statement. Both queries use the same ordered key range.
func (c *Core) normalizeColumn(ctx context.Context, table, keys, col, predicate string) (int64, error) {
	keyCols := strings.Split(keys, ",")
	bind := "(" + strings.TrimSuffix(strings.Repeat("?,", len(keyCols)), ",") + ")"
	key := "(" + keys + ")"
	desc := strings.Join(keyCols, " DESC, ") + " DESC"
	set := fmt.Sprintf("%[1]s=substr(%[1]s,1,19) || '.' || substr(rtrim(substr(%[1]s,21),'Z') || '000000000',1,9) || 'Z'", col)
	var total int64
	var cursor []any
	for {
		where := predicate
		if cursor != nil {
			where += " AND " + key + " > " + bind
		}
		page := "SELECT " + keys + " FROM " + table + " WHERE " + where + " ORDER BY " + keys + " LIMIT 512"
		last := make([]any, len(keyCols))
		dest := make([]any, len(keyCols))
		for i := range last {
			dest[i] = &last[i]
		}
		err := c.db.QueryRowContext(ctx, "SELECT "+keys+" FROM ("+page+") AS batch ORDER BY "+desc+" LIMIT 1", cursor...).Scan(dest...)
		if errors.Is(err, sql.ErrNoRows) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		update := "UPDATE " + table + " SET " + set + " WHERE " + where + " AND " + key + " <= " + bind
		count, err := c.execCount(ctx, update, append(cursor, last...)...)
		if err != nil {
			return total, err
		}
		total += count
		cursor = last
	}
}
