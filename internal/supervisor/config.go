package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
)

const configTemplate = `log_level: %s
device_name: teddycloud-spotify-shim
device_type: speaker

audio_backend: pipe
audio_output_pipe: %s
audio_output_pipe_format: s16le

credentials:
  type: interactive
  interactive:
    callback_port: 0

server:
  enabled: true
  address: 0.0.0.0
  port: 3678

zeroconf_enabled: false
normalisation_disabled: true
external_volume: true
`

// WriteConfig writes go-librespot's config.yml into configDir.
func WriteConfig(configDir, fifoPath, logLevel string) error {
	if err := os.MkdirAll(configDir, 0775); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	content := fmt.Sprintf(configTemplate, logLevel, fifoPath)
	path := filepath.Join(configDir, "config.yml")

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
