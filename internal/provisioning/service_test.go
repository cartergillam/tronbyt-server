package provisioning

import (
	"context"
	"testing"
	"time"

	"tronbyt-server/internal/data"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&data.User{}, &data.Device{}, &data.Household{}, &data.HouseholdMember{},
		&data.DeviceAssignment{}, &data.PairingCode{}, &data.MobileSession{},
	))
	require.NoError(t, db.Create(&data.User{Username: "owner-a", APIKey: "owner-a-key"}).Error)
	require.NoError(t, db.Create(&data.User{Username: "owner-b", APIKey: "owner-b-key"}).Error)
	require.NoError(t, db.Create(&data.Device{ID: "display-a", Username: "owner-a", APIKey: "device-a"}).Error)
	require.NoError(t, db.Create(&data.Device{ID: "display-b", Username: "owner-b", APIKey: "device-b"}).Error)
	service, err := NewService(db, "a-test-only-pairing-secret-that-is-long-enough")
	require.NoError(t, err)
	return service, db
}

func TestMemberProvisioningPairingReplayRevocationAndDeviceIsolation(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()
	member, err := service.CreateMember(ctx, "owner-a", "Household Member", []string{"display-a"})
	require.NoError(t, err)
	assert.Equal(t, "pending", member.Status)
	_, err = service.CreateMember(ctx, "owner-a", "Wrong Device", []string{"display-b"})
	assert.ErrorIs(t, err, ErrForbidden)

	code, _, err := service.CreatePairingCode(ctx, "owner-a", member.ID, 10*time.Minute)
	require.NoError(t, err)
	result, err := service.Redeem(ctx, code, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, []string{"display-a"}, result.DeviceIDs)
	_, err = service.Redeem(ctx, code, time.Hour)
	assert.ErrorIs(t, err, ErrCodeRedeemed)

	principal, err := service.Authenticate(ctx, result.SessionToken)
	require.NoError(t, err)
	assert.Equal(t, "owner-a", principal.OwnerUsername)
	assert.Equal(t, []string{"display-a"}, principal.DeviceIDs)
	assert.NotContains(t, principal.DeviceIDs, "display-b")
	require.NoError(t, service.Revoke(ctx, "owner-a", result.SessionID))
	_, err = service.Authenticate(ctx, result.SessionToken)
	assert.ErrorIs(t, err, ErrSessionInvalid)
}

func TestPairingCodeExpirationAndOwnerBoundary(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()
	member, err := service.CreateMember(ctx, "owner-a", "Member", []string{"display-a"})
	require.NoError(t, err)
	_, _, err = service.CreatePairingCode(ctx, "owner-b", member.ID, time.Minute)
	assert.ErrorIs(t, err, ErrForbidden)

	clock := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return clock }
	code, _, err := service.CreatePairingCode(ctx, "owner-a", member.ID, time.Minute)
	require.NoError(t, err)
	service.now = func() time.Time { return clock.Add(2 * time.Minute) }
	_, err = service.Redeem(ctx, code, time.Hour)
	assert.ErrorIs(t, err, ErrCodeInvalid)
}
