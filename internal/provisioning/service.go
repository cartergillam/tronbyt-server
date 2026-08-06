package provisioning

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"tronbyt-server/internal/data"

	"gorm.io/gorm"
)

var (
	ErrForbidden      = errors.New("provisioning operation is not permitted")
	ErrCodeInvalid    = errors.New("pairing code is invalid or expired")
	ErrCodeRedeemed   = errors.New("pairing code has already been redeemed")
	ErrSessionInvalid = errors.New("mobile session is invalid or revoked")
)

type Service struct {
	db            *gorm.DB
	pairingSecret []byte
	now           func() time.Time
}

type Principal struct {
	SessionID     string
	MemberID      string
	HouseholdID   string
	OwnerUsername string
	Role          string
	DeviceIDs     []string
}

type PairingResult struct {
	SessionToken string
	SessionID    string
	MemberID     string
	ExpiresAt    time.Time
	DeviceIDs    []string
}

func NewService(db *gorm.DB, pairingSecret string) (*Service, error) {
	if len(strings.TrimSpace(pairingSecret)) < 32 {
		return nil, errors.New("PAIRING_CODE_SECRET must contain at least 32 characters")
	}
	return &Service{db: db, pairingSecret: []byte(pairingSecret), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (service *Service) CreateMember(ctx context.Context, ownerUsername, displayName string, deviceIDs []string) (data.HouseholdMember, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || len(deviceIDs) == 0 {
		return data.HouseholdMember{}, errors.New("member name and at least one device are required")
	}
	member := data.HouseholdMember{ID: randomID("mem"), DisplayName: displayName, Role: "member", Status: "pending"}
	err := service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var household data.Household
		err := tx.Where("owner_username = ?", ownerUsername).First(&household).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			household = data.Household{ID: randomID("hh"), OwnerUsername: ownerUsername, Name: "Home"}
			if err := tx.Create(&household).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		member.HouseholdID = household.ID
		for _, deviceID := range uniqueStrings(deviceIDs) {
			var count int64
			if err := tx.Model(&data.Device{}).Where("id = ? AND username = ?", deviceID, ownerUsername).Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return ErrForbidden
			}
		}
		if err := tx.Create(&member).Error; err != nil {
			return err
		}
		for _, deviceID := range uniqueStrings(deviceIDs) {
			if err := tx.Create(&data.DeviceAssignment{MemberID: member.ID, DeviceID: deviceID}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return member, err
}

func (service *Service) CreatePairingCode(ctx context.Context, ownerUsername, memberID string, ttl time.Duration) (string, data.PairingCode, error) {
	if ttl <= 0 || ttl > 30*time.Minute {
		return "", data.PairingCode{}, errors.New("pairing code lifetime must be between zero and 30 minutes")
	}
	var member data.HouseholdMember
	if err := service.db.WithContext(ctx).Joins("JOIN households ON households.id = household_members.household_id").
		Where("household_members.id = ? AND households.owner_username = ?", memberID, ownerUsername).First(&member).Error; err != nil {
		return "", data.PairingCode{}, ErrForbidden
	}
	code, err := randomPairingCode()
	if err != nil {
		return "", data.PairingCode{}, err
	}
	record := data.PairingCode{
		ID: randomID("pair"), MemberID: member.ID, CodeHash: service.hashPairingCode(code), ExpiresAt: service.now().Add(ttl),
	}
	if err := service.db.WithContext(ctx).Create(&record).Error; err != nil {
		return "", data.PairingCode{}, err
	}
	return code, record, nil
}

func (service *Service) Redeem(ctx context.Context, code string, sessionTTL time.Duration) (PairingResult, error) {
	if sessionTTL <= 0 || sessionTTL > 180*24*time.Hour {
		return PairingResult{}, errors.New("mobile session lifetime is invalid")
	}
	now := service.now()
	var result PairingResult
	err := service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pairing data.PairingCode
		if err := tx.Where("code_hash = ?", service.hashPairingCode(code)).First(&pairing).Error; err != nil {
			return ErrCodeInvalid
		}
		if pairing.RedeemedAt != nil {
			return ErrCodeRedeemed
		}
		if !pairing.ExpiresAt.After(now) {
			return ErrCodeInvalid
		}
		update := tx.Model(&data.PairingCode{}).Where("id = ? AND redeemed_at IS NULL AND expires_at > ?", pairing.ID, now).Update("redeemed_at", &now)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrCodeRedeemed
		}
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return err
		}
		token := "tm_" + base64.RawURLEncoding.EncodeToString(tokenBytes)
		session := data.MobileSession{
			ID: randomID("sess"), MemberID: pairing.MemberID, TokenHash: hashSessionToken(token), ExpiresAt: now.Add(sessionTTL),
		}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		var assignments []data.DeviceAssignment
		if err := tx.Where("member_id = ?", pairing.MemberID).Find(&assignments).Error; err != nil {
			return err
		}
		result = PairingResult{SessionToken: token, SessionID: session.ID, MemberID: pairing.MemberID, ExpiresAt: session.ExpiresAt}
		for _, assignment := range assignments {
			result.DeviceIDs = append(result.DeviceIDs, assignment.DeviceID)
		}
		return tx.Model(&data.HouseholdMember{}).Where("id = ?", pairing.MemberID).Update("status", "active").Error
	})
	return result, err
}

func (service *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if !strings.HasPrefix(token, "tm_") {
		return Principal{}, ErrSessionInvalid
	}
	var session data.MobileSession
	if err := service.db.WithContext(ctx).Where("token_hash = ?", hashSessionToken(token)).First(&session).Error; err != nil {
		return Principal{}, ErrSessionInvalid
	}
	now := service.now()
	if session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return Principal{}, ErrSessionInvalid
	}
	var member data.HouseholdMember
	if err := service.db.WithContext(ctx).Where("id = ? AND status = ?", session.MemberID, "active").First(&member).Error; err != nil {
		return Principal{}, ErrSessionInvalid
	}
	var household data.Household
	if err := service.db.WithContext(ctx).Where("id = ?", member.HouseholdID).First(&household).Error; err != nil {
		return Principal{}, ErrSessionInvalid
	}
	var assignments []data.DeviceAssignment
	if err := service.db.WithContext(ctx).Where("member_id = ?", member.ID).Find(&assignments).Error; err != nil {
		return Principal{}, err
	}
	_ = service.db.WithContext(ctx).Model(&data.MobileSession{ID: session.ID}).Update("last_used_at", &now).Error
	principal := Principal{SessionID: session.ID, MemberID: member.ID, HouseholdID: household.ID, OwnerUsername: household.OwnerUsername, Role: member.Role}
	for _, assignment := range assignments {
		principal.DeviceIDs = append(principal.DeviceIDs, assignment.DeviceID)
	}
	return principal, nil
}

func (service *Service) Revoke(ctx context.Context, ownerUsername, sessionID string) error {
	now := service.now()
	result := service.db.WithContext(ctx).Model(&data.MobileSession{}).
		Where("mobile_sessions.id = ? AND member_id IN (?)", sessionID,
			service.db.Model(&data.HouseholdMember{}).Select("household_members.id").
				Joins("JOIN households ON households.id = household_members.household_id").Where("households.owner_username = ?", ownerUsername)).
		Update("revoked_at", &now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrForbidden
	}
	return nil
}

func (service *Service) hashPairingCode(code string) []byte {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	mac := hmac.New(sha256.New, service.pairingSecret)
	_, _ = mac.Write([]byte(normalized))
	return mac.Sum(nil)
}

func hashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func randomPairingCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buffer := make([]byte, 8)
	random := make([]byte, len(buffer))
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	for index := range buffer {
		buffer[index] = alphabet[int(random[index])%len(alphabet)]
	}
	return string(buffer[:4]) + "-" + string(buffer[4:]), nil
}

func randomID(prefix string) string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		panic(fmt.Sprintf("generate %s ID: %v", prefix, err))
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buffer)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
