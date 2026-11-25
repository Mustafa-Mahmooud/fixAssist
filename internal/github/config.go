package github

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The app's credentials can live in the database (github_app_config), saved
// by the manifest flow, so a hosted deployment needs no manual copying of the
// private key. Secrets are sealed with AES-256-GCM under a key derived from
// APP_SECRET, so a database dump alone doesn't reveal them.

func sealKey(appSecret []byte) []byte {
	m := hmac.New(sha256.New, appSecret)
	m.Write([]byte("fix-assistant github app config v1"))
	return m.Sum(nil)
}

func seal(appSecret, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(sealKey(appSecret))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func open(appSecret, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(sealKey(appSecret))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("github app config: sealed value too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

// SaveConfig stores the manifest conversion (replacing any earlier app).
func SaveConfig(ctx context.Context, pool *pgxpool.Pool, appSecret []byte, c Conversion) error {
	if _, err := ParseKey([]byte(c.PEM)); err != nil {
		return err
	}
	sec, err := seal(appSecret, []byte(c.ClientSecret))
	if err != nil {
		return err
	}
	key, err := seal(appSecret, []byte(c.PEM))
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `insert into github_app_config (id, app_id, slug, client_id, client_secret_enc, private_key_enc, html_url)
		values (1, $1, $2, $3, $4, $5, $6)
		on conflict (id) do update set app_id = excluded.app_id, slug = excluded.slug, client_id = excluded.client_id,
		  client_secret_enc = excluded.client_secret_enc, private_key_enc = excluded.private_key_enc,
		  html_url = excluded.html_url, created_at = now()`,
		c.ID, c.Slug, c.ClientID, sec, key, c.HTMLURL)
	return err
}

// LoadConfig returns the app saved by SaveConfig, or nil when there is none.
func LoadConfig(ctx context.Context, pool *pgxpool.Pool, appSecret []byte) (*App, error) {
	var (
		id                int64
		slug, clientID    string
		secretEnc, keyEnc []byte
	)
	err := pool.QueryRow(ctx, `select app_id, slug, client_id, client_secret_enc, private_key_enc from github_app_config where id = 1`).
		Scan(&id, &slug, &clientID, &secretEnc, &keyEnc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	secret, err := open(appSecret, secretEnc)
	if err != nil {
		return nil, fmt.Errorf("github app config: can't decrypt (was APP_SECRET changed?): %w", err)
	}
	pemText, err := open(appSecret, keyEnc)
	if err != nil {
		return nil, fmt.Errorf("github app config: can't decrypt (was APP_SECRET changed?): %w", err)
	}
	key, err := ParseKey(pemText)
	if err != nil {
		return nil, err
	}
	return &App{ID: id, Slug: slug, ClientID: clientID, ClientSecret: string(secret), Key: key}, nil
}
