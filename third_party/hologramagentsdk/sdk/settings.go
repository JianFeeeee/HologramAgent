package sdk

type SettingsAPI interface {
	// Get reads the plugin's own config value (config_<name> table).
	Get(key string) (interface{}, error)

	// Set writes a config value to the plugin's own config table.
	Set(key string, value interface{}) error

	// SetSecret writes a value that is ALWAYS encrypted at rest, regardless of
	// the key name. Use it when the key name is neutral (e.g. "blob", "conn")
	// and so cannot be detected as sensitive automatically.
	//
	// Returns an error (not a silent plaintext write) when no master key is
	// installed — a plugin that believes it stored a secret must not end up
	// with plaintext on disk.
	//
	// The companion read is GetSecret. ConfigDef.Secret/Type=="password" and
	// sensitive key names are encrypted automatically and do NOT need this.
	SetSecret(key string, value interface{}) error

	// GetSecret reads a value written by SetSecret (transparently decrypted).
	GetSecret(key string) (string, error)

	// List returns all keys matching the given prefix.
	List(prefix string) ([]string, error)

	// GetCore reads the core config table.
	GetCore(key string) (interface{}, error)

	// SetCore writes to the core config table.
	SetCore(key string, value interface{}) error

	// ListCore lists core config keys matching the prefix.
	ListCore(prefix string) ([]string, error)

	// DataDir returns the plugin-specific data directory (guaranteed to exist):
	// <daemon data>/plugin_data/<plugin_name>. Plugins should persist any
	// runtime files (generated images, caches, downloads) here.
	DataDir() string

	// GetPlugin reads another plugin's config table.
	GetPlugin(plugin, key string) (interface{}, error)

	// SetPlugin writes to another plugin's config table.
	SetPlugin(plugin, key string, value interface{}) error

	// ListPlugin lists another plugin's config keys matching the prefix.
	ListPlugin(plugin, prefix string) ([]string, error)

	// RegisterDef registers a config definition for UI display.
	RegisterDef(def ConfigDef)

	// Defs returns config definitions matching the prefix.
	Defs(prefix string) []*ConfigDef

	// Dump returns all config values.
	Dump() map[string]interface{}

	// Plugins returns a list of all plugin config namespaces.
	Plugins() []string
}

// ConfigDef describes a configuration field for the WebUI.
type ConfigDef struct {
	Key         string      `json:"key"`
	Default     interface{} `json:"default,omitempty"`
	Type        string      `json:"type"`
	DisplayName string      `json:"display_name"`
	Description string      `json:"description,omitempty"`
	Category    string      `json:"category,omitempty"`
	Options     []string    `json:"options,omitempty"`
	Min         float64     `json:"min,omitempty"`
	Max         float64     `json:"max,omitempty"`
	Step        float64     `json:"step,omitempty"`
	Required    bool        `json:"required,omitempty"`
	Secret      bool        `json:"secret,omitempty"`
}
