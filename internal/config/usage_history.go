package config

import "fmt"

// UsageHistoryConfig is independent of the short-lived usage export queue.
type UsageHistoryConfig struct {
	Disabled      bool   `yaml:"disabled" json:"disabled"`
	Path          string `yaml:"path" json:"path"`
	RetentionDays int    `yaml:"retention-days" json:"retention-days"`
}

func (c UsageHistoryConfig) Validate() error {
	if c.RetentionDays < 0 || c.RetentionDays > 365 {
		return fmt.Errorf("observability.usage.history.retention-days must be between 0 (default: 30) and 365")
	}
	return nil
}
