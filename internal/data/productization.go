package data

import "time"

// ProviderCredential stores only authenticated ciphertext. The deployment
// master key is deliberately external to the database and this model never
// serializes encrypted material through an API response.
type ProviderCredential struct {
	ID              string     `gorm:"primaryKey;size:96" json:"id"`
	Provider        string     `gorm:"index;size:64" json:"provider"`
	ScopeType       string     `gorm:"index:idx_provider_scope,priority:1;size:24" json:"scopeType"`
	ScopeID         string     `gorm:"index:idx_provider_scope,priority:2;size:128" json:"scopeID"`
	Ciphertext      []byte     `json:"-"`
	Nonce           []byte     `json:"-"`
	KeyVersion      uint       `json:"keyVersion"`
	ValidationState string     `gorm:"size:24" json:"validationState"`
	ValidatedAt     *time.Time `json:"validatedAt,omitempty"`
	LastUsedAt      *time.Time `json:"lastUsedAt,omitempty"`
	DisabledAt      *time.Time `json:"disabledAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type Household struct {
	ID            string    `gorm:"primaryKey;size:64" json:"id"`
	OwnerUsername string    `gorm:"uniqueIndex;size:128" json:"ownerUsername"`
	Name          string    `gorm:"size:128" json:"name"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type HouseholdMember struct {
	ID          string    `gorm:"primaryKey;size:64" json:"id"`
	HouseholdID string    `gorm:"index;size:64" json:"householdID"`
	DisplayName string    `gorm:"size:128" json:"displayName"`
	Role        string    `gorm:"size:24" json:"role"`
	Status      string    `gorm:"size:24" json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type DeviceAssignment struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	MemberID  string    `gorm:"uniqueIndex:idx_member_device,priority:1;size:64" json:"memberID"`
	DeviceID  string    `gorm:"uniqueIndex:idx_member_device,priority:2;size:128" json:"deviceID"`
	CreatedAt time.Time `json:"createdAt"`
}

type PairingCode struct {
	ID         string     `gorm:"primaryKey;size:64" json:"id"`
	MemberID   string     `gorm:"index;size:64" json:"memberID"`
	CodeHash   []byte     `gorm:"uniqueIndex" json:"-"`
	ExpiresAt  time.Time  `gorm:"index" json:"expiresAt"`
	RedeemedAt *time.Time `json:"redeemedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type MobileSession struct {
	ID         string     `gorm:"primaryKey;size:64" json:"id"`
	MemberID   string     `gorm:"index;size:64" json:"memberID"`
	TokenHash  []byte     `gorm:"uniqueIndex" json:"-"`
	ExpiresAt  time.Time  `gorm:"index" json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}
