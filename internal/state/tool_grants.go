package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ToolGrant is an operator's standing approval for a bounded number of calls
// of one tool until it expires.
type ToolGrant struct {
	GrantID   string
	Tool      string
	Agent     string // "" = any agent
	GrantedBy int64
	GrantedAt time.Time
	ExpiresAt time.Time
	MaxUses   int
	Uses      int
	RevokedAt time.Time
	RevokedBy int64
}

const toolGrantColumns = `grant_id, tool, agent, granted_by, granted_at, expires_at, max_uses, uses, revoked_at, revoked_by`

func scanToolGrant(scan func(...interface{}) error) (ToolGrant, error) {
	var g ToolGrant
	var granted, expires, revoked int64
	if err := scan(&g.GrantID, &g.Tool, &g.Agent, &g.GrantedBy, &granted, &expires, &g.MaxUses, &g.Uses, &revoked, &g.RevokedBy); err != nil {
		return ToolGrant{}, err
	}
	g.GrantedAt, g.ExpiresAt = time.Unix(granted, 0), time.Unix(expires, 0)
	if revoked != 0 {
		g.RevokedAt = time.Unix(revoked, 0)
	}
	return g, nil
}

// CreateToolGrant stores a new grant.
func (s *StateStore) CreateToolGrant(g ToolGrant) error {
	if s == nil || s.db == nil {
		return ErrNoStore
	}
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO tool_grants (grant_id, tool, agent, granted_by, granted_at, expires_at, max_uses) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		g.GrantID, g.Tool, g.Agent, g.GrantedBy, g.GrantedAt.Unix(), g.ExpiresAt.Unix(), g.MaxUses)
	return err
}

// UseToolGrant takes one use of a live grant that covers this tool and agent,
// the one expiring first, and returns it with the use counted. The use is
// taken in one conditional update, so two calls cannot share the last use.
// ok is false when no grant covers the call.
func (s *StateStore) UseToolGrant(tool, agent string, now time.Time) (ToolGrant, bool, error) {
	if s == nil || s.db == nil {
		return ToolGrant{}, false, ErrNoStore
	}
	for attempt := 0; attempt < 3; attempt++ {
		row := s.db.QueryRowContext(context.Background(),
			`SELECT `+toolGrantColumns+` FROM tool_grants
			 WHERE tool = ? AND (agent = '' OR agent = ?) AND revoked_at = 0 AND expires_at > ? AND uses < max_uses
			 ORDER BY expires_at, grant_id LIMIT 1`, tool, agent, now.Unix())
		g, err := scanToolGrant(row.Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ToolGrant{}, false, nil
		}
		if err != nil {
			return ToolGrant{}, false, err
		}
		res, err := s.db.ExecContext(context.Background(),
			`UPDATE tool_grants SET uses = uses + 1
			 WHERE grant_id = ? AND uses = ? AND revoked_at = 0 AND expires_at > ? AND uses < max_uses`,
			g.GrantID, g.Uses, now.Unix())
		if err != nil {
			return ToolGrant{}, false, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			g.Uses++
			return g, true, nil
		}
		// Another call took that use first; look again.
	}
	return ToolGrant{}, false, nil
}

// ToolGrants lists grants, newest first; live only unless all.
func (s *StateStore) ToolGrants(now time.Time, all bool) ([]ToolGrant, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	q := `SELECT ` + toolGrantColumns + ` FROM tool_grants`
	var args []interface{}
	if !all {
		q += ` WHERE revoked_at = 0 AND expires_at > ? AND uses < max_uses`
		args = append(args, now.Unix())
	}
	rows, err := s.db.QueryContext(context.Background(), q+` ORDER BY granted_at DESC, grant_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ToolGrant
	for rows.Next() {
		g, err := scanToolGrant(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeToolGrant ends a grant now. ok is false when no live grant has that id.
func (s *StateStore) RevokeToolGrant(id string, by int64, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, ErrNoStore
	}
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE tool_grants SET revoked_at = ?, revoked_by = ? WHERE grant_id = ? AND revoked_at = 0`, now.Unix(), by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
