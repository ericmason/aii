package cloudsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ericmason/aii/internal/cloudsync/remote"
)

// Config is the local sync configuration, stored at
// <dataDir>/sync/config.json with 0600 perms (it may hold S3
// credentials for unattended cron runs). dataDir is the DB's parent
// directory, so the config — like the lockfiles — follows AII_DB and
// each database gets its own repo binding.
type Config struct {
	Version    int    `json:"version"`
	RemoteType string `json:"remote_type"` // "s3" | "dir"

	// dir remote
	Path string `json:"path,omitempty"`

	// s3 remote
	Bucket          string `json:"bucket,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	Region          string `json:"region,omitempty"`
	PathStyle       bool   `json:"path_style,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`

	// Pins established at init/join, verified on every sync.
	RepoID      string `json:"repo_id"`
	Fingerprint string `json:"fingerprint"`
}

func syncDir(dataDir string) string    { return filepath.Join(dataDir, "sync") }
func ConfigPath(dataDir string) string { return filepath.Join(syncDir(dataDir), "config.json") }
func KeyPath(dataDir string) string    { return filepath.Join(syncDir(dataDir), "key.json") }

// ErrNotConfigured distinguishes "run `aii sync init` first" from a
// broken config.
var ErrNotConfigured = errors.New("sync is not configured — run `aii sync init --remote ...` first")

func LoadConfig(dataDir string) (*Config, error) {
	b, err := os.ReadFile(ConfigPath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(dataDir), err)
	}
	if c.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported config version %d (upgrade aii)", ConfigPath(dataDir), c.Version)
	}
	return &c, nil
}

func SaveConfig(dataDir string, c *Config) error {
	if err := os.MkdirAll(syncDir(dataDir), 0o700); err != nil {
		return err
	}
	c.Version = 1
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(dataDir), append(b, '\n'), 0o600)
}

// OpenRemote builds the Remote this config points at. S3 credentials
// resolve env-first so the config file never has to hold them:
// AII_SYNC_S3_* → standard AWS_* → config values.
func (c *Config) OpenRemote() (remote.Remote, error) {
	switch c.RemoteType {
	case "dir":
		return remote.NewDir(c.Path)
	case "s3":
		access := firstNonEmpty(os.Getenv("AII_SYNC_S3_ACCESS_KEY_ID"), os.Getenv("AWS_ACCESS_KEY_ID"), c.AccessKeyID)
		secret := firstNonEmpty(os.Getenv("AII_SYNC_S3_SECRET_ACCESS_KEY"), os.Getenv("AWS_SECRET_ACCESS_KEY"), c.SecretAccessKey)
		token := firstNonEmpty(os.Getenv("AII_SYNC_S3_SESSION_TOKEN"), os.Getenv("AWS_SESSION_TOKEN"))
		return remote.NewS3(remote.S3Config{
			Bucket:          c.Bucket,
			Prefix:          c.Prefix,
			Endpoint:        c.Endpoint,
			Region:          c.Region,
			PathStyle:       c.PathStyle,
			AccessKeyID:     access,
			SecretAccessKey: secret,
			SessionToken:    token,
		})
	default:
		return nil, fmt.Errorf("unknown remote type %q in config", c.RemoteType)
	}
}

// Describe is the human-facing remote descriptor with secrets elided.
func (c *Config) Describe() string {
	switch c.RemoteType {
	case "dir":
		return "dir:" + c.Path
	case "s3":
		s := "s3://" + c.Bucket
		if c.Prefix != "" {
			s += "/" + c.Prefix
		}
		if c.Endpoint != "" {
			s += " (" + c.Endpoint + ")"
		}
		return s
	default:
		return c.RemoteType
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
