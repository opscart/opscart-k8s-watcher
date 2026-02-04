package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ClusterConfig represents a single cluster entry
type ClusterConfig struct {
	Name    string `yaml:"name"`
	Context string `yaml:"context"`
	Group   string `yaml:"group"`
}

// OpsCartConfig represents the full config file
type OpsCartConfig struct {
	Clusters []ClusterConfig     `yaml:"clusters"`
	Groups   map[string][]string `yaml:"groups"`
}

// ConfigPaths returns global and local config paths
func ConfigPaths() (string, string) {
	home, _ := os.UserHomeDir()
	globalPath := filepath.Join(home, ".opscart", "config.yaml")
	localPath := ".opscart.yaml" // project-level override
	return globalPath, localPath
}

// LoadConfig loads config — local overrides global
func LoadConfig() (*OpsCartConfig, error) {
	globalPath, localPath := ConfigPaths()

	config := &OpsCartConfig{
		Groups: make(map[string][]string),
	}

	// Try global first
	if err := loadFile(globalPath, config); err != nil {
		// Global doesn't exist — that's okay
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("error reading global config (%s): %w", globalPath, err)
		}
	}

	// Try local — overrides global if exists
	localConfig := &OpsCartConfig{Groups: make(map[string][]string)}
	if err := loadFile(localPath, localConfig); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("error reading local config (%s): %w", localPath, err)
		}
	} else {
		// Local exists — use it as override
		config = localConfig
	}

	// If still empty, no config file found
	if len(config.Clusters) == 0 {
		return config, nil // return empty, let caller handle
	}

	// Auto-build groups if not defined
	config.buildGroupsIfEmpty()

	return config, nil
}

// loadFile reads a YAML file into the config struct
func loadFile(path string, config *OpsCartConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, config)
}

// buildGroupsIfEmpty auto-groups clusters by their 'group' field
func (c *OpsCartConfig) buildGroupsIfEmpty() {
	if len(c.Groups) > 0 {
		return
	}

	c.Groups = make(map[string][]string)
	for _, cluster := range c.Clusters {
		if cluster.Group != "" {
			c.Groups[cluster.Group] = append(c.Groups[cluster.Group], cluster.Name)
		}
	}
}

// GetClusterByName returns a cluster config by name
func (c *OpsCartConfig) GetClusterByName(name string) (*ClusterConfig, error) {
	for _, cluster := range c.Clusters {
		if cluster.Name == name || cluster.Context == name {
			return &cluster, nil
		}
	}
	return nil, fmt.Errorf("cluster '%s' not found in config", name)
}

// GetClustersByGroup returns all clusters in a group
func (c *OpsCartConfig) GetClustersByGroup(group string) ([]ClusterConfig, error) {
	names, exists := c.Groups[group]
	if !exists {
		return nil, fmt.Errorf("group '%s' not found. Available groups: %v", group, c.GroupNames())
	}

	var clusters []ClusterConfig
	for _, name := range names {
		cluster, err := c.GetClusterByName(name)
		if err != nil {
			continue
		}
		clusters = append(clusters, *cluster)
	}

	if len(clusters) == 0 {
		return nil, fmt.Errorf("no clusters found in group '%s'", group)
	}

	return clusters, nil
}

// GetAllClusters returns all configured clusters
func (c *OpsCartConfig) GetAllClusters() []ClusterConfig {
	return c.Clusters
}

// GroupNames returns available group names
func (c *OpsCartConfig) GroupNames() []string {
	var names []string
	for name := range c.Groups {
		names = append(names, name)
	}
	return names
}

// InitConfig creates the default config file structure
func InitConfig() error {
	globalPath, _ := ConfigPaths()

	// Create directory
	dir := filepath.Dir(globalPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Check if already exists
	if _, err := os.Stat(globalPath); err == nil {
		fmt.Printf("✅ Config already exists at: %s\n", globalPath)
		return nil
	}

	// Write sample config
	sample := `# OpsCart Multi-Cluster Configuration
# Global config: ~/.opscart/config.yaml
# You can also place .opscart.yaml in your project root (overrides global)

clusters:
  - name: prod-aks-01
    context: prod-aks-01-context
    group: production

  - name: prod-aks-02
    context: prod-aks-02-context
    group: production

  - name: staging-aks-01
    context: staging-aks-01-context
    group: staging

  - name: dev-aks-01
    context: dev-aks-01-context
    group: development

# Groups are auto-built from the 'group' field above.
# But you can also define custom groups manually:
#
# groups:
#   production:
#     - prod-aks-01
#     - prod-aks-02
#   staging:
#     - staging-aks-01
#   critical:           # custom group — can mix environments
#     - prod-aks-01
#     - staging-aks-01
`

	if err := os.WriteFile(globalPath, []byte(sample), 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	fmt.Printf("✅ Config created at: %s\n", globalPath)
	fmt.Println("📝 Edit the file and replace context names with your actual kubectl contexts")
	fmt.Println("💡 Run: kubectl config get-contexts   (to see your available contexts)")
	return nil
}

// PrintConfig displays the current loaded config
func (c *OpsCartConfig) PrintConfig() {
	fmt.Println("╔═══════════════════════════════════════════════════════════╗")
	fmt.Println("║           OPSCART CONFIGURATION                           ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════╝")
	fmt.Println()

	if len(c.Clusters) == 0 {
		fmt.Println("  ⚠️  No clusters configured")
		fmt.Println("  Run: ./opscart-scan config init")
		return
	}

	fmt.Printf("  📋 Clusters (%d):\n", len(c.Clusters))
	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Printf("  %-20s %-30s %-15s\n", "NAME", "CONTEXT", "GROUP")
	fmt.Println("  ─────────────────────────────────────────────")
	for _, cluster := range c.Clusters {
		fmt.Printf("  %-20s %-30s %-15s\n", cluster.Name, cluster.Context, cluster.Group)
	}

	fmt.Println()
	fmt.Printf("  📦 Groups (%d):\n", len(c.Groups))
	fmt.Println("  ─────────────────────────────────────────────")
	for group, clusters := range c.Groups {
		fmt.Printf("  %-15s → %v\n", group, clusters)
	}
	fmt.Println()
}
