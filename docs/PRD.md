# Product Requirements Document (PRD): teddycloud-spotify-radio-shim

## 1. High-Level System Scope
The **teddycloud-spotify-radio-shim** acts as a stateless middleware translator. Its core purpose is to intercept real-time physical playback events from a Teddycloud server and map them instantly to Spotify playback states, while proxying the resulting audio stream seamlessly back to Teddycloud.

---

## 2. Core Functional Requirements

### 2.1 Stateless Inbound Streaming (`/stream`)
* **Dynamic Stream Initialization:** The Bridge must expose a standard network endpoint that accepts a Spotify unique tracking identifier as a parameter (e.g., `GET /stream?spotify_uri=<URI>`).
* **Zero Configuration Storage:** The Bridge must **not** save, match, or maintain any internal database linking Tonie/Figurine IDs to Spotify playlists. The structural mapping is entirely externalized within Teddycloud’s figurine configuration files (`content.json`).
* **Continuous Audio Delivery:** Upon invocation of the stream endpoint, the Bridge must tell the Spotify sub-engine to play the targeted URI and output that continuous data stream directly back to the requesting HTTP client.

### 2.2 Real-Time Event Routing
* **Unified Log Ingestion:** The Bridge must monitor real-time playback state updates emitted from the Teddycloud server core.
* **Action Mapping:** The Bridge must monitor incoming events, matching specific physical hardware changes (RTNL logs) to immediate playback control triggers:
  * **Figurine Lifted:** Triggers a Spotify execution pause state.
  * **Figurine Placed Back:** Triggers a Spotify execution resume state.
  * **Right Side Slapped:** Triggers a Spotify skip-forward execution.
  * **Left Side Slapped:** Triggers a Spotify skip-backward execution.

### 2.3 Authentication Management
* **OAuth Lifecycle Handling:** The Bridge must expose a standard web route (e.g., `/login`). If the underlying Spotify sub-engine loses its connection token, this endpoint must display a clickable Spotify authorization link to a browser window.

---

## 3. Non-Functional Requirements & System Constraints

### 3.1 Strict Concurrency Ceiling (The Spotify Constraints)
* **Single Active Stream Enforcement:** Due to Spotify Premium account limitations, the system is strictly limited to handling **one active stream at a time**. 
* **Instance Replication Cap:** The deployment configuration must explicitly restrict service instances to exactly **one unit (`replicas: 1`)**. Horizontal auto-scaling is prohibited.
* **Hot-Swap Execution Serialization:** If a new HTTP connection request hits the stream endpoint while an older stream is actively transmitting data (e.g., swapping figurines rapidly), the system **must** immediately terminate the previous stream context before starting playback for the new request.

### 3.2 Metadata & Header Preservation (The Teddycloud Constraints)
* **Format Transparency:** The Bridge must forward the underlying audio streams with all container data headers (e.g., Audio Content-Type metadata) completely intact. The Bridge must not drop, truncate, or alter these headers, as Teddycloud relies on them to identify and decode the audio payload.
* **Backpressure Propagation:** When a pause event is active, the Bridge must drop data delivery speeds over the streaming channel to zero. This network block must pass backpressure down to the Spotify engine to naturally throttle memory use and stop internal buffers from deadlocking.

### 3.3 Platform & Persistence Targets (The OpenShift Constraints)
* **Single Pod Execution Boundary:** All parts of the bridge—including the event monitoring logic, web routing layer, and the underlying Spotify playback worker—must live and run inside a **single isolated OpenShift Pod**.
* **Volatile Memory Rule:** The application must not read or write raw audio bytes to the local host file system. All stream transits must happen completely within volatile RAM.
* **Persistent Session Cache:** The configuration path containing the authenticated Spotify connection session files must be explicitly bound to an OpenShift `PersistentVolumeClaim` (PVC). This keeps the device logged in across arbitrary pod restarts.
