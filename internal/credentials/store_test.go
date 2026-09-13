package credentials

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	"tronbyt-server/internal/data"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&data.ProviderCredential{}))
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	store, err := NewStore(db, base64.StdEncoding.EncodeToString(key))
	require.NoError(t, err)
	return store, db
}

func TestCredentialEncryptionRotationAndMetadataRedaction(t *testing.T) {
	store, db := testStore(t)
	ctx := context.Background()
	first, err := store.Put(ctx, "market-primary", "twelve-data", "household", "home-1", "first-secret")
	require.NoError(t, err)
	assert.Equal(t, uint(1), first.KeyVersion)

	var stored data.ProviderCredential
	require.NoError(t, db.First(&stored, "id = ?", "market-primary").Error)
	assert.NotContains(t, string(stored.Ciphertext), "first-secret")
	assert.NotEmpty(t, stored.Nonce)
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "secret")
	assert.NotContains(t, string(encoded), "ciphertext")

	resolved, err := store.Resolve(ctx, "market-primary", "household", "home-1")
	require.NoError(t, err)
	assert.Equal(t, "first-secret", resolved)

	rotated, err := store.Put(ctx, "market-primary", "twelve-data", "household", "home-1", "second-secret")
	require.NoError(t, err)
	assert.Equal(t, uint(2), rotated.KeyVersion)
	resolved, err = store.Resolve(ctx, "market-primary", "household", "home-1")
	require.NoError(t, err)
	assert.Equal(t, "second-secret", resolved)
}

func TestCredentialStoreTrimsPasteWhitespace(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	_, err := store.Put(ctx, "market-primary", "twelve-data", "server_owner", "owner", " \n  test-token\t")
	require.NoError(t, err)
	resolved, err := store.Resolve(ctx, "market-primary", "server_owner", "owner")
	require.NoError(t, err)
	assert.Equal(t, "test-token", resolved)
}

func TestCredentialScopeIsolationAndMissingMasterKey(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	_, err := store.Put(ctx, "weather", "openweather", "device", "display-a", "private")
	require.NoError(t, err)
	_, err = store.Resolve(ctx, "weather", "device", "display-b")
	assert.ErrorIs(t, err, ErrScopeMismatch)
	_, err = store.Put(ctx, "weather", "openweather", "device", "display-b", "replacement")
	assert.ErrorIs(t, err, ErrScopeMismatch)
	_, err = NewStore(nil, "")
	assert.ErrorIs(t, err, ErrMasterKeyUnavailable)
}

func TestCredentialListDisableEnableAndDelete(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	_, err := store.Put(ctx, "weather", "openweather", "server_owner", "owner", "private")
	require.NoError(t, err)
	items, err := store.List(ctx, "server_owner", "owner")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.True(t, items[0].Enabled)
	disabled, err := store.SetEnabled(ctx, "weather", "server_owner", "owner", false)
	require.NoError(t, err)
	assert.False(t, disabled.Enabled)
	_, err = store.Resolve(ctx, "weather", "server_owner", "owner")
	assert.ErrorIs(t, err, ErrCredentialDisabled)
	enabled, err := store.SetEnabled(ctx, "weather", "server_owner", "owner", true)
	require.NoError(t, err)
	assert.True(t, enabled.Enabled)
	require.NoError(t, store.Delete(ctx, "weather", "server_owner", "owner"))
	_, err = store.Metadata(ctx, "weather", "server_owner", "owner")
	assert.Error(t, err)
}

func TestOwnerMultipleMarketLabelsAndVersionedCacheScopes(t *testing.T) {
	store, db := testStore(t)
	for _, id := range []string{"carter-market", "ben-market"} {
		m, err := store.Put(t.Context(), id, "twelve-data", "server_owner", "owner", "offline-test", id+" label")
		require.NoError(t, err)
		require.Equal(t, id+" label", m.Label)
	}
	items, err := store.List(t.Context(), "server_owner", "owner")
	require.NoError(t, err)
	require.Len(t, items, 2)
	before, err := store.CredentialCacheScope(t.Context(), "ben-market", "server_owner", "owner")
	require.NoError(t, err)
	_, err = store.Put(t.Context(), "ben-market", "twelve-data", "server_owner", "owner", "offline-replacement")
	require.NoError(t, err)
	after, err := store.CredentialCacheScope(t.Context(), "ben-market", "server_owner", "owner")
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	_, err = store.SetEnabled(t.Context(), "ben-market", "server_owner", "owner", false)
	require.NoError(t, err)
	_, err = store.CredentialCacheScope(t.Context(), "ben-market", "server_owner", "owner")
	require.ErrorIs(t, err, ErrCredentialDisabled)
	var record data.ProviderCredential
	require.NoError(t, db.First(&record, "id = ?", "carter-market").Error)
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "offline-test")
	assert.NotContains(t, string(raw), "ciphertext")
	assert.NotContains(t, string(raw), "nonce")
}
