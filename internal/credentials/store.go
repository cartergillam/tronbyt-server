package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"tronbyt-server/internal/data"

	"gorm.io/gorm"
)

var (
	ErrMasterKeyUnavailable = errors.New("provider credential master key is unavailable")
	ErrScopeMismatch        = errors.New("provider credential scope mismatch")
	ErrCredentialMissing    = errors.New("provider credential is missing")
	ErrCredentialDisabled   = errors.New("provider credential is disabled")
)

type Metadata struct {
	ID              string     `json:"id"`
	Provider        string     `json:"provider"`
	ScopeType       string     `json:"scopeType"`
	ScopeID         string     `json:"scopeID"`
	KeyVersion      uint       `json:"keyVersion"`
	ValidationState string     `json:"validationState"`
	ValidatedAt     *time.Time `json:"validatedAt,omitempty"`
	LastUsedAt      *time.Time `json:"lastUsedAt,omitempty"`
	DisabledAt      *time.Time `json:"disabledAt,omitempty"`
	Enabled         bool       `json:"enabled"`
}

type Store struct {
	db   *gorm.DB
	aead cipher.AEAD
}

func NewStore(db *gorm.DB, encodedMasterKey string) (*Store, error) {
	key, err := decodeMasterKey(encodedMasterKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize provider credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize provider credential AEAD: %w", err)
	}
	return &Store{db: db, aead: aead}, nil
}

func decodeMasterKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, ErrMasterKeyUnavailable
	}
	decoders := []func(string) ([]byte, error){base64.StdEncoding.DecodeString, base64.RawStdEncoding.DecodeString, hex.DecodeString}
	for _, decode := range decoders {
		if key, err := decode(value); err == nil && len(key) == 32 {
			unique := map[byte]bool{}
			for _, item := range key {
				unique[item] = true
			}
			if len(unique) < 8 {
				return nil, errors.New("PROVIDER_CREDENTIAL_MASTER_KEY must be randomly generated, not a placeholder")
			}
			return key, nil
		}
	}
	return nil, errors.New("PROVIDER_CREDENTIAL_MASTER_KEY must encode exactly 32 random bytes")
}

func (s *Store) Put(ctx context.Context, id, provider, scopeType, scopeID, secret string) (Metadata, error) {
	if s == nil || s.aead == nil {
		return Metadata{}, ErrMasterKeyUnavailable
	}
	// Provider API keys are opaque tokens. Trim paste-only whitespace before
	// encrypting so a trailing newline cannot become part of an HTTP credential.
	secret = strings.TrimSpace(secret)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(provider) == "" || strings.TrimSpace(scopeType) == "" || strings.TrimSpace(scopeID) == "" || secret == "" {
		return Metadata{}, errors.New("credential ID, provider, scope and secret are required")
	}
	var existing data.ProviderCredential
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&existing).Error
	version := uint(1)
	if err == nil {
		if existing.ScopeType != scopeType || existing.ScopeID != scopeID || existing.Provider != provider {
			return Metadata{}, ErrScopeMismatch
		}
		version = existing.KeyVersion + 1
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Metadata{}, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Metadata{}, fmt.Errorf("generate credential nonce: %w", err)
	}
	aad := credentialAAD(id, provider, scopeType, scopeID, version)
	ciphertext := s.aead.Seal(nil, nonce, []byte(secret), aad)
	record := existing
	record.ID, record.Provider, record.ScopeType, record.ScopeID = id, provider, scopeType, scopeID
	record.Ciphertext, record.Nonce, record.KeyVersion = ciphertext, nonce, version
	record.ValidationState, record.ValidatedAt = "unvalidated", nil
	record.DisabledAt = nil
	if err := s.db.WithContext(ctx).Save(&record).Error; err != nil {
		return Metadata{}, err
	}
	return metadata(record), nil
}

func (s *Store) Resolve(ctx context.Context, id, scopeType, scopeID string) (string, error) {
	if s == nil || s.aead == nil {
		return "", ErrMasterKeyUnavailable
	}
	var record data.ProviderCredential
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrCredentialMissing
		}
		return "", err
	}
	if record.ScopeType != scopeType || record.ScopeID != scopeID {
		return "", ErrScopeMismatch
	}
	if record.DisabledAt != nil {
		return "", ErrCredentialDisabled
	}
	plaintext, err := s.aead.Open(nil, record.Nonce, record.Ciphertext, credentialAAD(record.ID, record.Provider, record.ScopeType, record.ScopeID, record.KeyVersion))
	if err != nil {
		return "", errors.New("provider credential could not be decrypted")
	}
	now := time.Now().UTC()
	_ = s.db.WithContext(ctx).Model(&data.ProviderCredential{ID: record.ID}).Update("last_used_at", &now).Error
	return string(plaintext), nil
}

func (s *Store) MarkValidation(ctx context.Context, id, state string) error {
	if state != "valid" && state != "invalid" && state != "unvalidated" {
		return errors.New("invalid credential validation state")
	}
	updates := map[string]any{"validation_state": state}
	if state == "valid" || state == "invalid" {
		now := time.Now().UTC()
		updates["validated_at"] = &now
	}
	return s.db.WithContext(ctx).Model(&data.ProviderCredential{ID: id}).Updates(updates).Error
}

func (s *Store) Metadata(ctx context.Context, id, scopeType, scopeID string) (Metadata, error) {
	var record data.ProviderCredential
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&record).Error; err != nil {
		return Metadata{}, err
	}
	if record.ScopeType != scopeType || record.ScopeID != scopeID {
		return Metadata{}, ErrScopeMismatch
	}
	return metadata(record), nil
}

func (s *Store) List(ctx context.Context, scopeType, scopeID string) ([]Metadata, error) {
	var records []data.ProviderCredential
	if err := s.db.WithContext(ctx).Where("scope_type = ? AND scope_id = ?", scopeType, scopeID).Order("provider ASC, id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]Metadata, 0, len(records))
	for _, record := range records {
		result = append(result, metadata(record))
	}
	return result, nil
}

func (s *Store) SetEnabled(ctx context.Context, id, scopeType, scopeID string, enabled bool) (Metadata, error) {
	var record data.ProviderCredential
	if err := s.db.WithContext(ctx).Where("id = ? AND scope_type = ? AND scope_id = ?", id, scopeType, scopeID).First(&record).Error; err != nil {
		return Metadata{}, ErrCredentialMissing
	}
	var disabledAt *time.Time
	if !enabled {
		now := time.Now().UTC()
		disabledAt = &now
	}
	if err := s.db.WithContext(ctx).Model(&data.ProviderCredential{ID: record.ID}).Update("disabled_at", disabledAt).Error; err != nil {
		return Metadata{}, err
	}
	record.DisabledAt = disabledAt
	return metadata(record), nil
}

func (s *Store) Delete(ctx context.Context, id, scopeType, scopeID string) error {
	result := s.db.WithContext(ctx).Where("id = ? AND scope_type = ? AND scope_id = ?", id, scopeType, scopeID).Delete(&data.ProviderCredential{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCredentialMissing
	}
	return nil
}

func metadata(record data.ProviderCredential) Metadata {
	return Metadata{
		ID: record.ID, Provider: record.Provider, ScopeType: record.ScopeType, ScopeID: record.ScopeID,
		KeyVersion: record.KeyVersion, ValidationState: record.ValidationState,
		ValidatedAt: record.ValidatedAt, LastUsedAt: record.LastUsedAt,
		DisabledAt: record.DisabledAt, Enabled: record.DisabledAt == nil,
	}
}

func credentialAAD(id, provider, scopeType, scopeID string, version uint) []byte {
	return []byte(fmt.Sprintf("tronbyt-provider-credential/v1\x00%s\x00%s\x00%s\x00%s\x00%d", id, provider, scopeType, scopeID, version))
}
