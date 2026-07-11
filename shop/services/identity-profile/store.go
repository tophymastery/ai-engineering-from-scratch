package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shop-platform/shop/libs/outbox"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (same one identity-auth uses)
)

//go:embed migrations/0001_profile.pg.sql
var pgMigration string

// PGSchema returns the production PostgreSQL migration (parity with identity-auth
// PGSchema()/libs/idempotency Schema()).
func PGSchema() string { return pgMigration }

// sqliteSchema is the SQLite twin of migrations/0001_profile.pg.sql (types only
// differ). PII columns stay `*_ct` and encrypted; the SQL is engine-agnostic.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS profiles (
    user_token   TEXT NOT NULL PRIMARY KEY,
    jurisdiction TEXT NOT NULL,
    full_name_ct TEXT,
    phone_ct     TEXT,
    email_ct     TEXT,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    erased_at    TIMESTAMP
);
CREATE TABLE IF NOT EXISTS addresses (
    addr_token   TEXT NOT NULL PRIMARY KEY,
    user_token   TEXT NOT NULL,
    jurisdiction TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT 'home',
    line1_ct     TEXT,
    city_ct      TEXT,
    postal_ct    TEXT,
    geo_ct       TEXT,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    erased_at    TIMESTAMP
);
CREATE INDEX IF NOT EXISTS addresses_user_idx ON addresses (user_token);
CREATE TABLE IF NOT EXISTS data_keys (
    user_token   TEXT NOT NULL PRIMARY KEY,
    wrapped_dek  TEXT,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    destroyed_at TIMESTAMP
);
`

// Sentinel store errors, mapped to 02 §2 codes by the handlers.
var (
	errNoProfile   = errors.New("no such profile")
	errProfileErased = errors.New("profile erased")
	errDupProfile  = errors.New("profile already exists")
)

// cellStore is ONE jurisdiction's PII store. `primary` is the live in-cell PII
// database + keystore; `backup` is a ciphertext-only replica standing in for the
// immutable/off-cell backup substrate (WAL archive, snapshot, DR copy). The
// backup deliberately holds NO keystore — keys live only in `primary`/KMS, which
// is what makes crypto-shredding possible against an immutable backup.
type cellStore struct {
	jurisdiction string
	primary      *sql.DB
	backup       *sql.DB
	ob           *outbox.SQLStore // transactional outbox bound to primary
}

// stores routes a request to the owning-jurisdiction cell store. A user's PII
// lives in exactly one cell (residency); cross-cell access is refused upstream.
type stores struct {
	kr    *keyring
	byJur map[string]*cellStore
}

func openStores(ctx context.Context, kr *keyring, jurisdictions []string) (*stores, error) {
	s := &stores{kr: kr, byJur: map[string]*cellStore{}}
	for _, j := range jurisdictions {
		cs, err := openCellStore(ctx, j)
		if err != nil {
			return nil, err
		}
		s.byJur[j] = cs
	}
	return s, nil
}

func (s *stores) close() {
	for _, cs := range s.byJur {
		_ = cs.primary.Close()
		_ = cs.backup.Close()
	}
}

// cell returns the owning cell store for a jurisdiction, or nil if this process
// is not homed for it (residency: a cell only serves its own jurisdiction).
func (s *stores) cell(j string) *cellStore { return s.byJur[j] }

func openCellStore(ctx context.Context, jurisdiction string) (*cellStore, error) {
	primary, err := openSQLite(ctx)
	if err != nil {
		return nil, err
	}
	backup, err := openSQLite(ctx)
	if err != nil {
		return nil, err
	}
	ob := outbox.NewSQLStore(primary, outbox.SQLiteDialect{})
	if err := outbox.Migrate(ctx, primary, outbox.SQLiteDialect{}); err != nil {
		return nil, fmt.Errorf("outbox migrate: %w", err)
	}
	return &cellStore{jurisdiction: jurisdiction, primary: primary, backup: backup, ob: ob}, nil
}

func openSQLite(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one shared in-memory db, serialized writer
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		return nil, fmt.Errorf("profile migrate: %w", err)
	}
	return db, nil
}

// --- domain records at the API boundary (plaintext; only ever in memory) ---

type addressInput struct {
	Label  string `json:"label"`
	Line1  string `json:"line1"`
	City   string `json:"city"`
	Postal string `json:"postal"`
	Geo    string `json:"geo"`
}

type addressView struct {
	AddrToken string `json:"addr_token"`
	Label     string `json:"label"`
	Line1     string `json:"line1,omitempty"`
	City      string `json:"city,omitempty"`
	Postal    string `json:"postal,omitempty"`
	Geo       string `json:"geo,omitempty"`
}

type profileInput struct {
	UserToken    string         `json:"user_token"`
	Jurisdiction string         `json:"jurisdiction"`
	FullName     string         `json:"full_name"`
	Phone        string         `json:"phone"`
	Email        string         `json:"email"`
	Addresses    []addressInput `json:"addresses"`
}

type profileView struct {
	UserToken    string        `json:"user_token"`
	Jurisdiction string        `json:"jurisdiction"`
	FullName     string        `json:"full_name,omitempty"`
	Phone        string        `json:"phone,omitempty"`
	Email        string        `json:"email,omitempty"`
	Addresses    []addressView `json:"addresses"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
	Erased       bool          `json:"erased"`
}

// createProfile encrypts every PII field under a fresh per-user DEK, writes the
// wrapped DEK to the keystore, the ciphertext to the primary store AND the
// backup replica, and appends a token-only profile.created event to the outbox —
// all in ONE primary transaction (transactional outbox, S-T6). It returns the
// address tokens minted (adr_...), which the event and the response carry.
func (cs *cellStore) createProfile(ctx context.Context, kr *keyring, in profileInput, ev *eventBuilder) (profileView, error) {
	dek := newDEK()
	wrapped, err := kr.wrapDEK(dek)
	if err != nil {
		return profileView{}, err
	}
	ut := in.UserToken

	nameCT, err := sealMaybe(dek, in.FullName, ut)
	if err != nil {
		return profileView{}, err
	}
	phoneCT, err := sealMaybe(dek, in.Phone, ut)
	if err != nil {
		return profileView{}, err
	}
	emailCT, err := sealMaybe(dek, in.Email, ut)
	if err != nil {
		return profileView{}, err
	}

	type addrRow struct {
		tok, label, l1, city, postal, geo string
	}
	var rows []addrRow
	var addrViews []addressView
	var addrTokens []string
	for _, a := range in.Addresses {
		at := newToken("adr")
		l1, _ := sealMaybe(dek, a.Line1, ut)
		city, _ := sealMaybe(dek, a.City, ut)
		postal, _ := sealMaybe(dek, a.Postal, ut)
		geo, _ := sealMaybe(dek, a.Geo, ut)
		label := a.Label
		if label == "" {
			label = "home"
		}
		rows = append(rows, addrRow{at, label, l1, city, postal, geo})
		addrViews = append(addrViews, addressView{AddrToken: at, Label: label, Line1: a.Line1, City: a.City, Postal: a.Postal, Geo: a.Geo})
		addrTokens = append(addrTokens, at)
	}

	tx, err := cs.primary.BeginTx(ctx, nil)
	if err != nil {
		return profileView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO data_keys (user_token, wrapped_dek) VALUES (?, ?)`, ut, wrapped); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return profileView{}, errDupProfile
		}
		return profileView{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO profiles (user_token, jurisdiction, full_name_ct, phone_ct, email_ct) VALUES (?, ?, ?, ?, ?)`,
		ut, cs.jurisdiction, nameCT, phoneCT, emailCT); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return profileView{}, errDupProfile
		}
		return profileView{}, err
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO addresses (addr_token, user_token, jurisdiction, label, line1_ct, city_ct, postal_ct, geo_ct)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.tok, ut, cs.jurisdiction, r.label, r.l1, r.city, r.postal, r.geo); err != nil {
			return profileView{}, err
		}
	}
	// Transactional outbox: token-only profile.created event (D3 — events carry
	// usr_/adr_ tokens, never PII).
	env := ev.profileChanged("profile.updated", "created", ut, cs.jurisdiction, addrTokens)
	if err := cs.ob.WriteInTx(ctx, tx, "profile.updated", env); err != nil {
		return profileView{}, err
	}
	if err := tx.Commit(); err != nil {
		return profileView{}, err
	}

	// Replicate ciphertext to the immutable-backup substrate (no keystore there).
	if err := cs.replicateToBackup(ctx, ut); err != nil {
		return profileView{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	return profileView{
		UserToken: ut, Jurisdiction: cs.jurisdiction,
		FullName: in.FullName, Phone: in.Phone, Email: in.Email,
		Addresses: addrViews, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func sealMaybe(dek []byte, v, aad string) (string, error) {
	if v == "" {
		return "", nil
	}
	return sealField(dek, v, aad)
}

// replicateToBackup copies the CURRENT ciphertext rows (profiles + addresses) for
// a user into the backup store. Idempotent (INSERT OR REPLACE). The backup never
// receives data_keys — that is the whole point of crypto-shredding.
func (cs *cellStore) replicateToBackup(ctx context.Context, ut string) error {
	var jur, nameCT, phoneCT, emailCT sql.NullString
	err := cs.primary.QueryRowContext(ctx,
		`SELECT jurisdiction, full_name_ct, phone_ct, email_ct FROM profiles WHERE user_token = ?`, ut).
		Scan(&jur, &nameCT, &phoneCT, &emailCT)
	if err != nil {
		return err
	}
	if _, err := cs.backup.ExecContext(ctx,
		`INSERT OR REPLACE INTO profiles (user_token, jurisdiction, full_name_ct, phone_ct, email_ct) VALUES (?, ?, ?, ?, ?)`,
		ut, jur, nameCT, phoneCT, emailCT); err != nil {
		return err
	}
	rows, err := cs.primary.QueryContext(ctx,
		`SELECT addr_token, jurisdiction, label, line1_ct, city_ct, postal_ct, geo_ct FROM addresses WHERE user_token = ?`, ut)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var at, aj, label string
		var l1, city, postal, geo sql.NullString
		if err := rows.Scan(&at, &aj, &label, &l1, &city, &postal, &geo); err != nil {
			return err
		}
		if _, err := cs.backup.ExecContext(ctx,
			`INSERT OR REPLACE INTO addresses (addr_token, user_token, jurisdiction, label, line1_ct, city_ct, postal_ct, geo_ct)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			at, ut, aj, label, l1, city, postal, geo); err != nil {
			return err
		}
	}
	return rows.Err()
}

// loadDEK fetches and unwraps a user's DEK from the primary keystore. Returns
// errKeyDestroyed once the key has been crypto-shredded, errNoProfile if absent.
func (cs *cellStore) loadDEK(ctx context.Context, kr *keyring, ut string) ([]byte, error) {
	var wrapped sql.NullString
	err := cs.primary.QueryRowContext(ctx,
		`SELECT wrapped_dek FROM data_keys WHERE user_token = ?`, ut).Scan(&wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoProfile
	}
	if err != nil {
		return nil, err
	}
	if !wrapped.Valid || wrapped.String == "" {
		return nil, errKeyDestroyed
	}
	return kr.unwrapDEK(wrapped.String)
}

// getProfile reads + decrypts a user's profile from the PRIMARY store.
func (cs *cellStore) getProfile(ctx context.Context, kr *keyring, ut string) (profileView, error) {
	return cs.decryptFrom(ctx, kr, cs.primary, ut)
}

// getProfileFromBackup decrypts from the BACKUP store (used by the erasure proof
// to show backup PII is unreadable after shredding — the DEK still comes from the
// one keystore, which is gone).
func (cs *cellStore) getProfileFromBackup(ctx context.Context, kr *keyring, ut string) (profileView, error) {
	return cs.decryptFrom(ctx, kr, cs.backup, ut)
}

func (cs *cellStore) decryptFrom(ctx context.Context, kr *keyring, db *sql.DB, ut string) (profileView, error) {
	var jur string
	var nameCT, phoneCT, emailCT sql.NullString
	var createdAt, updatedAt string
	var erasedAt sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT jurisdiction, full_name_ct, phone_ct, email_ct, created_at, updated_at, erased_at
		   FROM profiles WHERE user_token = ?`, ut).
		Scan(&jur, &nameCT, &phoneCT, &emailCT, &createdAt, &updatedAt, &erasedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return profileView{}, errNoProfile
	}
	if err != nil {
		return profileView{}, err
	}
	// Load + unwrap the DEK. Post-erasure this returns errKeyDestroyed and NO
	// plaintext is ever produced — the crypto-shred guarantee.
	dek, err := cs.loadDEK(ctx, kr, ut)
	if err != nil {
		return profileView{}, err
	}
	pv := profileView{UserToken: ut, Jurisdiction: jur, CreatedAt: createdAt, UpdatedAt: updatedAt, Erased: erasedAt.Valid}
	if pv.FullName, err = openMaybe(dek, nameCT, ut); err != nil {
		return profileView{}, err
	}
	if pv.Phone, err = openMaybe(dek, phoneCT, ut); err != nil {
		return profileView{}, err
	}
	if pv.Email, err = openMaybe(dek, emailCT, ut); err != nil {
		return profileView{}, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT addr_token, label, line1_ct, city_ct, postal_ct, geo_ct FROM addresses WHERE user_token = ? ORDER BY addr_token`, ut)
	if err != nil {
		return profileView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var at, label string
		var l1, city, postal, geo sql.NullString
		if err := rows.Scan(&at, &label, &l1, &city, &postal, &geo); err != nil {
			return profileView{}, err
		}
		av := addressView{AddrToken: at, Label: label}
		if av.Line1, err = openMaybe(dek, l1, ut); err != nil {
			return profileView{}, err
		}
		if av.City, err = openMaybe(dek, city, ut); err != nil {
			return profileView{}, err
		}
		if av.Postal, err = openMaybe(dek, postal, ut); err != nil {
			return profileView{}, err
		}
		if av.Geo, err = openMaybe(dek, geo, ut); err != nil {
			return profileView{}, err
		}
		pv.Addresses = append(pv.Addresses, av)
	}
	return pv, rows.Err()
}

func openMaybe(dek []byte, ct sql.NullString, aad string) (string, error) {
	if !ct.Valid || ct.String == "" {
		return "", nil
	}
	return openField(dek, ct.String, aad)
}

// addAddress appends an address (new adr_ token) to an existing profile,
// encrypting under the user's DEK and replicating to backup + emitting an event.
func (cs *cellStore) addAddress(ctx context.Context, kr *keyring, ut string, a addressInput, ev *eventBuilder) (addressView, error) {
	dek, err := cs.loadDEK(ctx, kr, ut)
	if err != nil {
		return addressView{}, err
	}
	at := newToken("adr")
	l1, _ := sealMaybe(dek, a.Line1, ut)
	city, _ := sealMaybe(dek, a.City, ut)
	postal, _ := sealMaybe(dek, a.Postal, ut)
	geo, _ := sealMaybe(dek, a.Geo, ut)
	label := a.Label
	if label == "" {
		label = "home"
	}
	tx, err := cs.primary.BeginTx(ctx, nil)
	if err != nil {
		return addressView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO addresses (addr_token, user_token, jurisdiction, label, line1_ct, city_ct, postal_ct, geo_ct)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, at, ut, cs.jurisdiction, label, l1, city, postal, geo); err != nil {
		return addressView{}, err
	}
	env := ev.profileChanged("profile.updated", "address_added", ut, cs.jurisdiction, []string{at})
	if err := cs.ob.WriteInTx(ctx, tx, "profile.updated", env); err != nil {
		return addressView{}, err
	}
	if err := tx.Commit(); err != nil {
		return addressView{}, err
	}
	if err := cs.replicateToBackup(ctx, ut); err != nil {
		return addressView{}, err
	}
	return addressView{AddrToken: at, Label: label, Line1: a.Line1, City: a.City, Postal: a.Postal, Geo: a.Geo}, nil
}

// updateProfile re-encrypts the mutable PII fields under the user's EXISTING DEK
// (only fields whose pointer is non-nil are changed), replicates to backup and
// emits a token-only profile.updated event.
func (cs *cellStore) updateProfile(ctx context.Context, kr *keyring, ut string, in profileInput, ev *eventBuilder) (profileView, error) {
	dek, err := cs.loadDEK(ctx, kr, ut)
	if err != nil {
		return profileView{}, err
	}
	nameCT, _ := sealMaybe(dek, in.FullName, ut)
	phoneCT, _ := sealMaybe(dek, in.Phone, ut)
	emailCT, _ := sealMaybe(dek, in.Email, ut)
	tx, err := cs.primary.BeginTx(ctx, nil)
	if err != nil {
		return profileView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE profiles SET full_name_ct = ?, phone_ct = ?, email_ct = ?, updated_at = CURRENT_TIMESTAMP WHERE user_token = ? AND erased_at IS NULL`,
		nameCT, phoneCT, emailCT, ut); err != nil {
		return profileView{}, err
	}
	env := ev.profileChanged("profile.updated", "updated", ut, cs.jurisdiction, nil)
	if err := cs.ob.WriteInTx(ctx, tx, "profile.updated", env); err != nil {
		return profileView{}, err
	}
	if err := tx.Commit(); err != nil {
		return profileView{}, err
	}
	if err := cs.replicateToBackup(ctx, ut); err != nil {
		return profileView{}, err
	}
	return cs.getProfile(ctx, kr, ut)
}

// erasureReceipt records a crypto-shred erasure for the audit trail / DPO log.
type erasureReceipt struct {
	UserToken    string   `json:"user_token"`
	Jurisdiction string   `json:"jurisdiction"`
	Erased       bool     `json:"erased"`
	KeyDestroyed bool     `json:"key_destroyed"`
	StoresShred  []string `json:"stores"`
	ErasedAt     string   `json:"erased_at"`
	AddrTokens   []string `json:"addr_tokens"`
}

// erase performs crypto-shredding: it destroys the wrapped DEK in the keystore
// (the ONE place a decryptable key exists) and stamps erased_at on the profile in
// BOTH the primary and the backup stores. It emits a token-only profile.erased
// event. After this returns, every PII ciphertext for the user — primary, backup,
// and any older event/WAL copy — is permanently unreadable, yet the usr_/adr_
// tokens remain valid references so token-keyed order history still replays.
func (cs *cellStore) erase(ctx context.Context, ut string, ev *eventBuilder) (erasureReceipt, error) {
	// Collect address tokens first (for the event + receipt).
	var addrTokens []string
	rows, err := cs.primary.QueryContext(ctx, `SELECT addr_token FROM addresses WHERE user_token = ?`, ut)
	if err != nil {
		return erasureReceipt{}, err
	}
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			rows.Close()
			return erasureReceipt{}, err
		}
		addrTokens = append(addrTokens, at)
	}
	rows.Close()

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	tx, err := cs.primary.BeginTx(ctx, nil)
	if err != nil {
		return erasureReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE data_keys SET wrapped_dek = NULL, destroyed_at = ? WHERE user_token = ? AND wrapped_dek IS NOT NULL`,
		nowStr, ut)
	if err != nil {
		return erasureReceipt{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Already shredded, or no such user. Distinguish.
		var exists int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM data_keys WHERE user_token = ?`, ut).Scan(&exists)
		if exists == 0 {
			return erasureReceipt{}, errNoProfile
		}
		// idempotent re-erase — fall through, still stamp erased_at + emit.
	}
	if _, err := tx.ExecContext(ctx, `UPDATE profiles SET erased_at = ?, full_name_ct = NULL, phone_ct = NULL, email_ct = NULL WHERE user_token = ?`, nowStr, ut); err != nil {
		return erasureReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE addresses SET erased_at = ? WHERE user_token = ?`, nowStr, ut); err != nil {
		return erasureReceipt{}, err
	}
	env := ev.profileChanged("profile.erased", "erased", ut, cs.jurisdiction, addrTokens)
	if err := cs.ob.WriteInTx(ctx, tx, "profile.erased", env); err != nil {
		return erasureReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return erasureReceipt{}, err
	}

	// Stamp the backup tombstone too. The backup keeps its ciphertext (immutable
	// substrate) — we do NOT and NEED NOT scrub it; it is already unreadable.
	if _, err := cs.backup.ExecContext(ctx, `UPDATE profiles SET erased_at = ? WHERE user_token = ?`, nowStr, ut); err != nil {
		return erasureReceipt{}, err
	}
	if _, err := cs.backup.ExecContext(ctx, `UPDATE addresses SET erased_at = ? WHERE user_token = ?`, nowStr, ut); err != nil {
		return erasureReceipt{}, err
	}

	return erasureReceipt{
		UserToken: ut, Jurisdiction: cs.jurisdiction, Erased: true, KeyDestroyed: true,
		StoresShred: []string{"primary", "backup"}, ErasedAt: nowStr, AddrTokens: addrTokens,
	}, nil
}

// tokenRef is the NON-PII resolution of a usr_/adr_ token: enough for the order
// path to validate/route a token, never any personal data.
type tokenRef struct {
	Token        string `json:"token"`
	Kind         string `json:"kind"`
	Jurisdiction string `json:"jurisdiction"`
	Erased       bool   `json:"erased"`
	Exists       bool   `json:"exists"`
}

// resolveToken maps a usr_/adr_ token to its non-PII reference. Order replay uses
// this to confirm a token is a real, in-cell reference; it survives erasure (the
// tombstone row keeps the token + jurisdiction) so token-keyed history replays.
func (cs *cellStore) resolveToken(ctx context.Context, tok string) (tokenRef, error) {
	ref := tokenRef{Token: tok, Kind: tokenKind(tok), Jurisdiction: cs.jurisdiction}
	switch ref.Kind {
	case "user":
		var erasedAt sql.NullString
		err := cs.primary.QueryRowContext(ctx, `SELECT erased_at FROM profiles WHERE user_token = ?`, tok).Scan(&erasedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ref, nil
		}
		if err != nil {
			return ref, err
		}
		ref.Exists, ref.Erased = true, erasedAt.Valid
	case "address":
		var erasedAt sql.NullString
		err := cs.primary.QueryRowContext(ctx, `SELECT erased_at FROM addresses WHERE addr_token = ?`, tok).Scan(&erasedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ref, nil
		}
		if err != nil {
			return ref, err
		}
		ref.Exists, ref.Erased = true, erasedAt.Valid
	}
	return ref, nil
}

// rawBackupCiphertext returns the encrypted full_name column straight from the
// backup store (bypassing any decrypt) — used by the erasure proof to show the
// ciphertext BLOB physically survives in the backup while being unreadable.
func (cs *cellStore) rawBackupCiphertext(ctx context.Context, ut string) (string, error) {
	var ct sql.NullString
	err := cs.backup.QueryRowContext(ctx, `SELECT full_name_ct FROM profiles WHERE user_token = ?`, ut).Scan(&ct)
	if err != nil {
		return "", err
	}
	return ct.String, nil
}
