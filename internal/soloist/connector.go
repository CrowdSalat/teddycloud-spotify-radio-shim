package soloist

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/gorilla/websocket"
)

// CommandConnector serialises writes to the Soloist WebSocket connection.
type CommandConnector struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *CommandConnector) attach(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *CommandConnector) detach() {
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
}

func (c *CommandConnector) write(raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return errors.New("soloist: not connected")
	}

	return c.conn.WriteMessage(websocket.TextMessage, raw)
}

type playCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	URI     string `json:"uri"`
}

// Play sends a play command with the given Spotify URI.
func (c *CommandConnector) Play(uri string) error {
	cmd, err := json.Marshal(playCommand{
		Type:    "command",
		Command: "play",
		URI:     uri,
	})
	if err != nil {
		return err
	}

	return c.write(cmd)
}

type simpleCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

func (c *CommandConnector) sendSimple(command string) error {
	cmd, err := json.Marshal(simpleCommand{
		Type:    "command",
		Command: command,
	})
	if err != nil {
		return err
	}

	return c.write(cmd)
}

// Pause pauses playback.
func (c *CommandConnector) Pause() error {
	return c.sendSimple("pause")
}

// SkipNext skips to the next track.
func (c *CommandConnector) SkipNext() error {
	return c.sendSimple("skip_next")
}

// SkipPrev skips to the previous track.
func (c *CommandConnector) SkipPrev() error {
	return c.sendSimple("skip_prev")
}
