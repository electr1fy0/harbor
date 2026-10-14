package model

import "time"

// DataServiceSummary is the safe API view of a managed data service. Storage
// paths and credentials deliberately stay out of this model.
type DataServiceSummary struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Version       string        `json:"version"`
	State         ServiceState  `json:"state"`
	CreatedAt     time.Time     `json:"createdAt"`
	Bindings      []DataBinding `json:"bindings"`
	DatabaseBytes int64         `json:"databaseBytes"`
	WALBytes      int64         `json:"walBytes"`
	Backups       []DataBackup  `json:"backups"`
	BackupPolicy  *BackupPolicy `json:"backupPolicy,omitempty"`
}

// DataBinding is a safe view of an application/service link.
type DataBinding struct {
	ID            string    `json:"id"`
	AppName       string    `json:"appName"`
	ServiceID     string    `json:"serviceId"`
	CreatedAt     time.Time `json:"createdAt"`
	DatabaseBytes int64     `json:"databaseBytes"`
	WALBytes      int64     `json:"walBytes"`
}

// DataBackup describes a consistent SQLite snapshot and its verification
// status. File paths are never exposed.
type DataBackup struct {
	ID           string     `json:"id"`
	ServiceID    string     `json:"serviceId"`
	ServiceName  string     `json:"serviceName"`
	AppName      string     `json:"appName"`
	SizeBytes    int64      `json:"sizeBytes"`
	CreatedAt    time.Time  `json:"createdAt"`
	Scheduled    bool       `json:"scheduled"`
	Verification string     `json:"verification"`
	VerifiedAt   *time.Time `json:"verifiedAt,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// BackupPolicy controls service-scoped scheduled snapshots.
type BackupPolicy struct {
	ServiceID string    `json:"serviceId"`
	Frequency string    `json:"frequency"`
	Retention int       `json:"retention"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DataServiceExport contains an encrypted, bounded Harbor archive encoded for
// the JSON API. The content itself is opaque to Harbor after encryption.
type DataServiceExport struct {
	FileName      string `json:"fileName"`
	ContentType   string `json:"contentType"`
	EncryptedData string `json:"encryptedData"`
	SizeBytes     int64  `json:"sizeBytes"`
}
