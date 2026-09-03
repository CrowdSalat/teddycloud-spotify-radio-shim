# Requirements: teddycloud-spotify-radio-shim

## 1. Purpose

A Toniebox is a children's audio player. It is controlled by physical interactions: placing a figurine on top to play, lifting it to pause, slapping the ears to skip. A Toniebox does not connect to Spotify directly. It streams audio over HTTP from a server.

This service bridges that gap. It makes Spotify content reachable from a Toniebox via Teddycloud. The figurine triggers playback; the Toniebox receives an audio stream; the physical controls work.

---

## 2. Scope Boundaries

This service is a translator and proxy. It has no UI for end users. It does not manage a library or a catalog. It does not handle user accounts. It connects two existing systems — Teddycloud and Spotify — and makes them work together.

---

## 3. Music Service

### 3.1 Spotify only

The service must use **Spotify** as the music source. No other streaming service is in scope.

**Why Spotify:** The target users already have Spotify Premium accounts. Spotify provides the catalog they want to use with their Toniebox.

### 3.2 Spotify Premium required

The service requires a **Spotify Premium** account. Free accounts do not support the API capabilities needed to control playback programmatically.

### 3.3 One active stream at a time

Only **one stream** can be active at any moment. This is a hard limit of Spotify: one account can stream to one device at a time.

---

## 4. Audio Delivery

### 4.1 HTTP audio stream endpoint

The service must expose an HTTP endpoint that Teddycloud can point a figurine configuration at. When Teddycloud fetches that URL, the service must start streaming audio for the configured Spotify URI and keep streaming until the connection closes.

### 4.2 Spotify URI as input

The Spotify content to play (track, album, playlist, podcast) is identified by a **Spotify URI**. The URI is passed to the stream endpoint by Teddycloud as a parameter.

**Why the URI comes from Teddycloud:** Teddycloud already stores the figurine-to-content mapping. This service does not duplicate that mapping. Each figurine's configuration in Teddycloud contains the Spotify URI for that figurine. The service has no database of its own.

### 4.3 Audio format compatible with Teddycloud

The audio stream must be in a format that Teddycloud and the Toniebox can decode and play. The format must be declared correctly in the HTTP response so Teddycloud knows how to handle it.

### 4.4 Stream must start promptly

When Teddycloud requests the stream, playback must begin within a few seconds. Children do not wait.

### 4.5 Swapping figurines replaces the stream

If Teddycloud requests a new stream while one is already active (different Spotify URI), the old stream must stop and the new one must start. There must be no service restart. No manual intervention. This happens automatically.

---

## 5. Playback Control

Physical interactions on the Toniebox produce events. Teddycloud captures these events. The service must listen to Teddycloud and translate them into Spotify playback actions.

| Physical action | Spotify action |
|---|---|
| Figurine placed on Toniebox | Resume playback |
| Figurine lifted off Toniebox | Pause playback |
| Right ear slap | Skip to next track |
| Left ear slap | Skip to previous track |

**Why these controls matter:** The Toniebox has no screen and no keyboard. These four physical gestures are the only way a child interacts with it. If they do not work, the device is not usable with Spotify.

### 5.1 Source of control events

The service must receive control events from **Teddycloud**. The exact protocol (SSE, MQTT, etc.) is a design decision, not a requirement.

---

## 6. Authentication

### 6.1 Spotify login is required once

The service must support a one-time Spotify authorization flow. After the user authorizes the service, the session must persist across restarts without re-authorizing.

**Why persistence matters:** The service runs as a container. Containers restart. Forcing a re-login after every restart is not acceptable.

### 6.2 Login method

The preferred login method is **static credentials in configuration** (e.g. environment variables or a config file). This avoids any interactive flow entirely.

If the Spotify API or the chosen library does not support static credentials, a **terminal-based flow** (e.g. a one-time login command, a printed URL to open manually) is acceptable.

A browser-based OAuth flow exposed via HTTP is a last resort. It is more complex to operate in a headless container environment.

---

## 7. Deployment

### 7.1 Runs as a container

The service must run as a **container image**. It is deployed alongside Teddycloud, which itself runs in a container.

### 7.2 Single instance only

Exactly **one instance** of this service may run at a time. Horizontal scaling is not allowed. This follows from the one-stream-at-a-time constraint (§ 3.3).

---

## 8. Out of Scope

These are explicitly not requirements for this service:

- **Multi-user support.** One Spotify account, one household.
- **Playlist management.** Users configure Spotify URIs in Teddycloud, not here.
- **Volume control.** Volume is handled by the Toniebox hardware.
- **Spotify account creation or subscription management.**
- **Support for Spotify Free accounts.**
- **Any streaming service other than Spotify.**
- **A graphical user interface.**
