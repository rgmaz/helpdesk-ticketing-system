package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// UserRepo provides access to the users and roles tables.
type UserRepo struct{ db *sql.DB }

// userColumns is the canonical projection, aliased to the Go struct field names.
// It must keep its trailing newline: these fragments are concatenated, so a
// missing one would splice the last column into the FROM clause.
const userColumns = `
	u.id, u.username, u.email, u.password_hash, u.full_name, r.name,
	u.active, u.created_at, u.updated_at
`

const userFrom = `
FROM users u JOIN roles r ON r.id = u.role_id`

// Create inserts a user, hashing the password first.
func (r *UserRepo) Create(ctx context.Context, in NewUser) (models.User, error) {
	role, err := models.ParseRole(string(in.Role))
	if err != nil {
		return models.User{}, fmt.Errorf("%w: %s", models.ErrInvalidInput, err)
	}
	var roleID int64
	if err := r.db.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = ?`, role).Scan(&roleID); err != nil {
		return models.User{}, IsNotFound(err)
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO users (username, email, password_hash, full_name, role_id, active)
		VALUES (?, ?, ?, ?, ?, ?)`,
		in.Username, in.Email, in.PasswordHash, in.FullName, roleID, boolToInt(in.Active))
	if err != nil {
		return models.User{}, wrapValidation(err, "username or email")
	}
	id, _ := res.LastInsertId()
	return r.ByID(ctx, id)
}

// NewUser is the input for Create.
type NewUser struct {
	Username     string
	Email        string
	PasswordHash string
	FullName     string
	Role         models.RoleName
	Active       bool
}

// ByID loads a single user.
func (r *UserRepo) ByID(ctx context.Context, id int64) (models.User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+userFrom+` WHERE u.id = ?`, id)
	return scanUser(row)
}

// ByUsername loads a user by their unique username (used at login).
func (r *UserRepo) ByUsername(ctx context.Context, username string) (models.User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+userFrom+` WHERE u.username = ?`, username)
	return scanUser(row)
}

// ByEmail loads a user by their unique email address.
func (r *UserRepo) ByEmail(ctx context.Context, email string) (models.User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+userFrom+` WHERE u.email = ?`, email)
	return scanUser(row)
}

// ByIdentifier resolves a login attempt that supplies either a username or an
// email address.
func (r *UserRepo) ByIdentifier(ctx context.Context, identifier string) (models.User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+userFrom+
		` WHERE u.username = ? OR u.email = ?`, identifier, strings.ToLower(identifier))
	return scanUser(row)
}

// UserFilter narrows a user listing.
type UserFilter struct {
	Role    models.RoleName
	Active  *bool
	Search  string
	Page    int
	Limit   int
	SortBy  string
	SortDir string
}

// List returns a page of users plus the total matching count.
func (r *UserRepo) List(ctx context.Context, f UserFilter) ([]models.User, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Role != "" {
		where = append(where, "r.name = ?")
		args = append(args, string(f.Role))
	}
	if f.Active != nil {
		where = append(where, "u.active = ?")
		args = append(args, boolToInt(*f.Active))
	}
	if f.Search != "" {
		where = append(where, "(u.username LIKE ? OR u.email LIKE ? OR u.full_name LIKE ?)")
		like := "%" + f.Search + "%"
		args = append(args, like, like, like)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) `+userFrom+` WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Whitelist sort columns: never interpolate user input into SQL.
	orderBy := map[string]string{
		"username": "u.username ASC",
		"name":     "u.full_name ASC",
		"role":     "r.name ASC",
		"newest":   "u.id DESC",
	}[f.SortBy]
	if orderBy == "" {
		orderBy = "u.id ASC"
	}
	if strings.EqualFold(f.SortDir, "desc") {
		orderBy = strings.Replace(orderBy, " ASC", " DESC", 1)
	}

	q := `SELECT ` + userColumns + userFrom + ` WHERE ` + clause + ` ORDER BY ` + orderBy
	if f.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, f.Page*f.Limit)
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	users := []models.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, u)
	}
	return users, total, rows.Err()
}

// AllStaff returns active technicians and admins, used by the routing algorithm
// and the workload report.
func (r *UserRepo) AllStaff(ctx context.Context) ([]models.User, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+userColumns+userFrom+
		` WHERE r.name IN ('technician','admin') AND u.active = 1 ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := []models.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// Technicians returns only technicians (the pool that automated routing draws
// from), optionally excluding accounts that are marked unavailable.
func (r *UserRepo) Technicians(ctx context.Context) ([]models.User, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+userColumns+userFrom+
		` WHERE r.name = 'technician' AND u.active = 1 ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := []models.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// RoleID returns the numeric id for a role name.
func (r *UserRepo) RoleID(ctx context.Context, name models.RoleName) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = ?`, name).Scan(&id)
	return id, IsNotFound(err)
}

// RoleIDForUser returns the numeric role id of a specific user.
func (r *UserRepo) RoleIDForUser(userID int64) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(context.Background(), `SELECT role_id FROM users WHERE id = ?`, userID).Scan(&id)
	return id, IsNotFound(err)
}

// UpdateUser carries optional field changes; nil pointers mean "leave as is".
type UpdateUser struct {
	Email        *string
	FullName     *string
	Active       *bool
	PasswordHash *string
	Role         *models.RoleName
}

// Update changes mutable profile fields and optionally the password.
func (r *UserRepo) Update(ctx context.Context, id int64, in UpdateUser) (models.User, error) {
	sets := []string{"updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')"}
	args := []any{}

	if in.Email != nil {
		sets = append(sets, "email = ?")
		args = append(args, *in.Email)
	}
	if in.FullName != nil {
		sets = append(sets, "full_name = ?")
		args = append(args, *in.FullName)
	}
	if in.Active != nil {
		sets = append(sets, "active = ?")
		args = append(args, boolToInt(*in.Active))
	}
	if in.PasswordHash != nil {
		sets = append(sets, "password_hash = ?")
		args = append(args, *in.PasswordHash)
	}
	if in.Role != nil {
		roleID, err := r.RoleID(ctx, *in.Role)
		if err != nil {
			return models.User{}, err
		}
		sets = append(sets, "role_id = ?")
		args = append(args, roleID)
	}

	args = append(args, id)
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return models.User{}, wrapValidation(err, "email")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return models.User{}, models.ErrNotFound
	}
	return r.ByID(ctx, id)
}

// Delete removes a user. Tickets reference users, so the FK guard will reject
// deletion when history exists; deactivating is the preferred alternative.
func (r *UserRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			return fmt.Errorf("%w: user has tickets or comments and cannot be deleted; deactivate instead", models.ErrConflict)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return models.ErrNotFound
	}
	return nil
}

// Count returns the total number of accounts.
func (r *UserRepo) Count(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// Roles lists all roles in display order.
func (r *UserRepo) Roles(ctx context.Context) ([]models.Role, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, description FROM roles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := []models.Role{}
	for rows.Next() {
		var role models.Role
		if err := rows.Scan(&role.ID, &role.Name, &role.Description); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

// scanner abstracts *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanUser(s scanner) (models.User, error) {
	var (
		u      models.User
		role   string
		active int
	)
	err := s.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.FullName, &role,
		&active, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return models.User{}, IsNotFound(err)
	}
	parsed, perr := models.ParseRole(role)
	if perr != nil {
		return models.User{}, fmt.Errorf("user %d has invalid role %q: %w", u.ID, role, perr)
	}
	u.Role = parsed
	u.Active = active == 1
	return u, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
