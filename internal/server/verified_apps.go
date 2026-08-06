package server

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// verifiedAppsJSON is maintained independently of upstream app manifests.
// An app is verified only when its canonical ID is explicitly present here.
//
//go:embed verified_apps.json
var verifiedAppsJSON []byte

type verifiedAppMetadata struct {
	Verified                       bool           `json:"verified"`
	Recommended                    bool           `json:"recommended"`
	VerifiedVersion                string         `json:"verifiedVersion"`
	VerificationDate               string         `json:"verificationDate"`
	CompatibilityNotes             string         `json:"compatibilityNotes"`
	PreferredConfigurationDefaults map[string]any `json:"preferredConfigurationDefaults,omitempty"`
}

var verifiedAppsManifest = mustLoadVerifiedAppsManifest()

func mustLoadVerifiedAppsManifest() map[string]verifiedAppMetadata {
	var manifest map[string]verifiedAppMetadata
	if err := json.Unmarshal(verifiedAppsJSON, &manifest); err != nil {
		panic(fmt.Sprintf("decode embedded verified-app manifest: %v", err))
	}
	for id, metadata := range manifest {
		if id == "" || !metadata.Verified || metadata.VerifiedVersion == "" || metadata.VerificationDate == "" {
			panic("verified-app manifest contains an incomplete entry")
		}
	}
	return manifest
}

func verifiedAppsRevision() string {
	sum := sha256.Sum256(verifiedAppsJSON)
	return hex.EncodeToString(sum[:8])
}

func verifiedMetadataFor(id string) (verifiedAppMetadata, bool) {
	metadata, ok := verifiedAppsManifest[id]
	return metadata, ok
}

type starterBundle struct {
	ID     string
	Name   string
	AppIDs []string
}

var verifiedStarterBundles = map[string]starterBundle{
	"verified-starter": {
		ID: "verified-starter", Name: "Verified Starter Apps",
		// MLB requires an explicit team choice and is therefore verified but not
		// safe for unattended bundle installation.
		AppIDs: []string{"og-clock", "cfl-scores", "quote-of-the-day"},
	},
}
