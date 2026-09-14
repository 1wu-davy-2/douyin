package spark

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"douyin/backend/internal/db"
)

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	ErrNotFound          = errors.New("spark: not found")
	ErrDuplicateAccount  = errors.New("spark: account already exists")
	ErrInvalidFriendList = errors.New("spark: engine returned an empty friend list")
)

// Account is one spark_accounts row.
type Account struct {
	ID                   int64  `json:"id"`
	UniqueID             string `json:"unique_id"`
	Username             string `json:"username"`
	Nickname             string `json:"nickname"`
	ProfileName          string `json:"profile_name"`
	Enabled              bool   `json:"enabled"`
	Status               string `json:"status"`
	LastError            string `json:"last_error"`
	LastFriendsRefreshAt string `json:"last_friends_refresh_at"`
	LastSendAt           string `json:"last_send_at"`
	CooldownUntil        string `json:"cooldown_until"`
	FailureCountToday    string `json:"failure_count_today"` // raw JSON {"date": n}
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

// Friend is one spark_friends row.
type Friend struct {
	ID          int64  `json:"id"`
	AccountID   int64  `json:"account_id"`
	FriendKey   string `json:"friend_key"`
	DisplayName string `json:"display_name"`
	Selected    bool   `json:"selected"`
}

// FriendToday is a friend row enriched with its latest send state today
// ("" = none, "strong" / "failed").
type FriendToday struct {
	Friend
	TodayState string `json:"today_state"`
}

// FriendUpdate is one PATCH /accounts/{id}/friends entry.
type FriendUpdate struct {
	Key      string `json:"key"`
	Selected bool   `json:"selected"`
}

// SendRecord is one spark_send_records row (list view joins the account's
// display name for the records table).
type SendRecord struct {
	ID           int64  `json:"id"`
	AccountID    int64  `json:"account_id"`
	AccountLabel string `json:"account_label"`
	FriendKey    string `json:"friend_key"`
	Message      string `json:"message"`
	ConfirmState string `json:"confirm_state"`
	Category     string `json:"category"`
	Detail       string `json:"detail"`
	RunMode      string `json:"run_mode"`
	SentAt       string `json:"sent_at"`
	LocalDate    string `json:"local_date"`
}

// DayStats aggregates confirm states for one account/day.
type DayStats struct {
	Strong int `json:"strong"`
	Weak   int `json:"weak"`
	Failed int `json:"failed"`
}

// Store owns the four spark tables.
type Store struct {
	handle *sql.DB
}

// NewStore builds a store over the shared SQLite handle.
func NewStore(handle *sql.DB) *Store { return &Store{handle: handle} }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------- accounts --

const accountColumns = `id, unique_id, username, nickname, profile_name, enabled,
	status, last_error, last_friends_refresh_at, last_send_at, cooldown_until,
	failure_count_today, created_at, updated_at`

func scanAccount(scan func(...any) error) (Account, error) {
	var a Account
	var enabled int
	err := scan(&a.ID, &a.UniqueID, &a.Username, &a.Nickname, &a.ProfileName, &enabled,
		&a.Status, &a.LastError, &a.LastFriendsRefreshAt, &a.LastSendAt, &a.CooldownUntil,
		&a.FailureCountToday, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Account{}, err
	}
	a.Enabled = enabled != 0
	return a, nil
}

// CreateAccount inserts a new account. unique_id is unique; duplicates map to
// ErrDuplicateAccount.
func (s *Store) CreateAccount(uniqueID, username, nickname, profileName string) (Account, error) {
	now := nowRFC3339()
	res, err := s.handle.Exec(`
		INSERT INTO spark_accounts (unique_id, username, nickname, profile_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		uniqueID, username, nickname, profileName, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: spark_accounts.unique_id") {
			return Account{}, ErrDuplicateAccount
		}
		return Account{}, fmt.Errorf("spark: create account: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Account{}, err
	}
	return s.GetAccount(id)
}

// GetAccount fetches one account by id (ErrNotFound when missing).
func (s *Store) GetAccount(id int64) (Account, error) {
	row := s.handle.QueryRow(
		`SELECT `+accountColumns+` FROM spark_accounts WHERE id = ?`, id)
	a, err := scanAccount(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// ListAccounts returns all accounts ordered by id.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.handle.Query(
		`SELECT ` + accountColumns + ` FROM spark_accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("spark: list accounts: %w", err)
	}
	defer rows.Close()
	// 非 nil 保证 JSON 序列化为 [] 而非 null(前端契约:列表恒为数组)。
	out := make([]Account, 0)
	for rows.Next() {
		a, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccountPatch is the partial update accepted by UpdateAccount.
type AccountPatch struct {
	Enabled  *bool
	Nickname *string
}

// UpdateAccount applies the patch and bumps updated_at.
func (s *Store) UpdateAccount(id int64, patch AccountPatch) (Account, error) {
	sets := []string{"updated_at = ?"}
	args := []any{nowRFC3339()}
	if patch.Enabled != nil {
		sets = append(sets, "enabled = ?")
		if *patch.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if patch.Nickname != nil {
		sets = append(sets, "nickname = ?")
		args = append(args, *patch.Nickname)
	}
	args = append(args, id)
	res, err := s.handle.Exec(
		`UPDATE spark_accounts SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return Account{}, fmt.Errorf("spark: update account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Account{}, ErrNotFound
	}
	return s.GetAccount(id)
}

// DeleteAccount removes the account; friends/records cascade.
func (s *Store) DeleteAccount(id int64) error {
	res, err := s.handle.Exec(`DELETE FROM spark_accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("spark: delete account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAccountRuntime updates status (+ last_error) and bumps updated_at.
// Event emission stays in the service layer.
func (s *Store) SetAccountRuntime(id int64, status, lastError string) error {
	_, err := s.handle.Exec(`
		UPDATE spark_accounts SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, lastError, nowRFC3339(), id)
	if err != nil {
		return fmt.Errorf("spark: set account status: %w", err)
	}
	return nil
}

// SetAccountTimestamp stamps last_send_at or last_friends_refresh_at.
func (s *Store) SetAccountTimestamp(id int64, column, rfc3339 string) error {
	// column is a call-site constant, never user input.
	if column != "last_send_at" && column != "last_friends_refresh_at" {
		return fmt.Errorf("spark: bad timestamp column %q", column)
	}
	_, err := s.handle.Exec(
		`UPDATE spark_accounts SET `+column+` = ?, updated_at = ? WHERE id = ?`,
		rfc3339, nowRFC3339(), id)
	if err != nil {
		return fmt.Errorf("spark: set %s: %w", column, err)
	}
	return nil
}

// RecordAccountFailure bumps the account's failure counter for localDate and
// — when the counter reaches attempts — sets cooldown_until. Returns cooled
// so the caller can pick the follow-up status (cooldown vs error).
func (s *Store) RecordAccountFailure(accountID int64, localDate string, attempts, cooldownMinutes int, nowLocal time.Time) (bool, error) {
	var raw string
	err := s.handle.QueryRow(
		`SELECT failure_count_today FROM spark_accounts WHERE id = ?`, accountID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("spark: read failure counter: %w", err)
	}
	counts := map[string]int{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &counts) // corrupt JSON = start fresh
	}
	counts[localDate]++
	encoded, err := json.Marshal(counts)
	if err != nil {
		encoded = []byte("{}")
	}
	if counts[localDate] >= attempts {
		until := nowLocal.Add(time.Duration(cooldownMinutes) * time.Minute).Format(time.RFC3339)
		_, err = s.handle.Exec(`
			UPDATE spark_accounts
			SET failure_count_today = ?, cooldown_until = ?, status = 'cooldown', updated_at = ?
			WHERE id = ?`,
			string(encoded), until, nowRFC3339(), accountID)
		return true, err
	}
	_, err = s.handle.Exec(`
		UPDATE spark_accounts SET failure_count_today = ?, updated_at = ? WHERE id = ?`,
		string(encoded), nowRFC3339(), accountID)
	return false, err
}

// ResetStuckSending returns accounts stranded in status='sending' (crash
// mid-run) to idle at startup. Returns the number of repaired rows.
func (s *Store) ResetStuckSending() (int64, error) {
	res, err := s.handle.Exec(`
		UPDATE spark_accounts SET status = 'idle', updated_at = ? WHERE status = 'sending'`,
		nowRFC3339())
	if err != nil {
		return 0, fmt.Errorf("spark: reset stuck sending: %w", err)
	}
	return res.RowsAffected()
}

// ----------------------------------------------------------------- friends --

// ReplaceFriends swaps the account's friend list for the engine snapshot.
// selected flags of already-known friend keys are preserved; brand-new keys
// default to selected=0 (never auto-send to a new friend). One transaction;
// returns (total, newly added).
func (s *Store) ReplaceFriends(accountID int64, fetched []FriendPair) (int, int, error) {
	if len(fetched) == 0 {
		return 0, 0, ErrInvalidFriendList
	}
	total, newCount := 0, 0
	err := db.WithTx(context.Background(), s.handle, func(tx *sql.Tx) error {
		// Snapshot key -> selected before the delete; only these keys keep
		// their flag, everything else comes back unselected.
		rows, err := tx.Query(
			`SELECT friend_key, selected FROM spark_friends WHERE account_id = ?`, accountID)
		if err != nil {
			return err
		}
		prevSelected := map[string]bool{}
		for rows.Next() {
			var key string
			var sel int
			if err := rows.Scan(&key, &sel); err != nil {
				rows.Close()
				return err
			}
			prevSelected[key] = sel != 0
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		if _, err := tx.Exec(`DELETE FROM spark_friends WHERE account_id = ?`, accountID); err != nil {
			return err
		}
		total, newCount = 0, 0
		for _, f := range fetched {
			key := strings.TrimSpace(f.Key)
			if key == "" {
				continue
			}
			sel := 0
			known := false
			if sel0, ok := prevSelected[key]; ok {
				known = true
				if sel0 {
					sel = 1
				}
			}
			if !known {
				newCount++
			}
			if _, err := tx.Exec(`
				INSERT INTO spark_friends (account_id, friend_key, display_name, selected)
				VALUES (?, ?, ?, ?)`,
				accountID, key, strings.TrimSpace(f.DisplayName), sel); err != nil {
				return err
			}
			total++
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("spark: replace friends: %w", err)
	}
	return total, newCount, nil
}

// ListFriends returns the account's friends ordered by id. selected filters
// when non-nil; todayState (per-friend latest confirm state for localDate)
// is merged in when localDate is non-empty.
func (s *Store) ListFriends(accountID int64, selected *bool, localDate string) ([]FriendToday, error) {
	q := `SELECT f.id, f.account_id, f.friend_key, f.display_name, f.selected FROM spark_friends f WHERE f.account_id = ?`
	args := []any{accountID}
	if selected != nil {
		q += ` AND f.selected = ?`
		if *selected {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	q += ` ORDER BY f.id`
	rows, err := s.handle.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("spark: list friends: %w", err)
	}
	defer rows.Close()
	out := make([]FriendToday, 0) // 非 nil → JSON []
	for rows.Next() {
		var f FriendToday
		var sel int
		if err := rows.Scan(&f.ID, &f.AccountID, &f.FriendKey, &f.DisplayName, &sel); err != nil {
			return nil, err
		}
		f.Selected = sel != 0
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if localDate != "" && len(out) > 0 {
		states, err := s.TodayFriendStates(accountID, localDate)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].TodayState = states[out[i].FriendKey]
		}
	}
	return out, nil
}

// PatchFriends applies per-key selected flags. Unknown keys are ignored
// (the UI only sends visible rows).
func (s *Store) PatchFriends(accountID int64, updates []FriendUpdate) error {
	err := db.WithTx(context.Background(), s.handle, func(tx *sql.Tx) error {
		for _, u := range updates {
			sel := 0
			if u.Selected {
				sel = 1
			}
			if _, err := tx.Exec(
				`UPDATE spark_friends SET selected = ? WHERE account_id = ? AND friend_key = ?`,
				sel, accountID, u.Key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("spark: patch friends: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------- records --

// InsertSendRecords appends send records (one tx, best-effort per batch).
func (s *Store) InsertSendRecords(recs []SendRecord) error {
	if len(recs) == 0 {
		return nil
	}
	err := db.WithTx(context.Background(), s.handle, func(tx *sql.Tx) error {
		for _, r := range recs {
			if _, err := tx.Exec(`
				INSERT INTO spark_send_records
					(account_id, friend_key, message, confirm_state, category, detail, run_mode, sent_at, local_date)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				r.AccountID, r.FriendKey, r.Message, r.ConfirmState, r.Category, r.Detail,
				r.RunMode, r.SentAt, r.LocalDate); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("spark: insert send records: %w", err)
	}
	return nil
}

// ListSendRecords pages records newest-first by id. cursor=0 means "from the
// newest"; nextCursor is 0 when the page is the last one.
func (s *Store) ListSendRecords(accountID *int64, cursor int64, limit int) ([]SendRecord, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	q := `
		SELECT r.id, r.account_id, COALESCE(a.nickname, a.username, a.unique_id, ''),
		       r.friend_key, r.message, r.confirm_state, r.category, r.detail,
		       r.run_mode, r.sent_at, r.local_date
		FROM spark_send_records r
		LEFT JOIN spark_accounts a ON a.id = r.account_id
		WHERE 1=1`
	var args []any
	if accountID != nil {
		q += ` AND r.account_id = ?`
		args = append(args, *accountID)
	}
	if cursor > 0 {
		q += ` AND r.id < ?`
		args = append(args, cursor)
	}
	q += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.handle.Query(q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("spark: list send records: %w", err)
	}
	defer rows.Close()
	out := make([]SendRecord, 0) // 非 nil → JSON []
	for rows.Next() {
		var r SendRecord
		if err := rows.Scan(&r.ID, &r.AccountID, &r.AccountLabel, &r.FriendKey, &r.Message,
			&r.ConfirmState, &r.Category, &r.Detail, &r.RunMode, &r.SentAt, &r.LocalDate); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// TodayStats aggregates one account's confirm states for localDate.
func (s *Store) TodayStats(accountID int64, localDate string) (DayStats, error) {
	rows, err := s.handle.Query(`
		SELECT confirm_state, COUNT(*) FROM spark_send_records
		WHERE account_id = ? AND local_date = ? GROUP BY confirm_state`,
		accountID, localDate)
	if err != nil {
		return DayStats{}, fmt.Errorf("spark: today stats: %w", err)
	}
	defer rows.Close()
	var st DayStats
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return DayStats{}, err
		}
		switch state {
		case "strong":
			st.Strong = n
		case "weak":
			st.Weak = n
		default:
			st.Failed += n
		}
	}
	return st, rows.Err()
}

// TodayFriendStates maps friend_key -> latest confirm state for the day
// (latest = highest record id; retries overwrite earlier outcomes).
func (s *Store) TodayFriendStates(accountID int64, localDate string) (map[string]string, error) {
	rows, err := s.handle.Query(`
		SELECT friend_key, confirm_state, category FROM spark_send_records
		WHERE account_id = ? AND local_date = ? ORDER BY id`,
		accountID, localDate)
	if err != nil {
		return nil, fmt.Errorf("spark: today friend states: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, state, category string
		if err := rows.Scan(&key, &state, &category); err != nil {
			return nil, err
		}
		if out[key] == "strong" {
			// Strong is sticky: a failed retry after a confirmed send must
			// not downgrade the day's outcome.
			continue
		}
		out[key] = stateWithCategory(state, category)
	}
	return out, rows.Err()
}

func stateWithCategory(state, category string) string {
	if state == "strong" {
		return "strong"
	}
	if category == "" {
		return state
	}
	return state + ":" + category
}

// LastStrongMessages returns the newest strong message per friend for the
// day — the engine's anti-duplicate seed (previous_messages).
func (s *Store) LastStrongMessages(accountID int64, localDate string) (map[string]string, error) {
	rows, err := s.handle.Query(`
		SELECT friend_key, message FROM spark_send_records
		WHERE account_id = ? AND local_date = ? AND confirm_state = 'strong' ORDER BY id`,
		accountID, localDate)
	if err != nil {
		return nil, fmt.Errorf("spark: last strong messages: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, msg string
		if err := rows.Scan(&key, &msg); err != nil {
			return nil, err
		}
		out[key] = msg
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- settings --

// LoadSendConfig reads spark_settings['send_config'] with default fallback.
func (s *Store) LoadSendConfig() (SendConfig, error) {
	var raw string
	err := s.handle.QueryRow(
		`SELECT value FROM spark_settings WHERE key = 'send_config'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultSendConfig(), nil
	}
	if err != nil {
		return SendConfig{}, fmt.Errorf("spark: load send_config: %w", err)
	}
	return ParseSendConfig(raw), nil
}

// SaveSendConfig replaces the stored config (whole-object PUT contract).
func (s *Store) SaveSendConfig(cfg SendConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("spark: encode send_config: %w", err)
	}
	_, err = s.handle.Exec(`
		INSERT INTO spark_settings (key, value) VALUES ('send_config', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		string(raw))
	if err != nil {
		return fmt.Errorf("spark: save send_config: %w", err)
	}
	return nil
}
