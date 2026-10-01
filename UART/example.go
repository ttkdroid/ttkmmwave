package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"go.bug.st/serial"
)

const (
	packetSize = 30
	headByte0  = 0xAA
	headByte1  = 0xFF
	tailByte0  = 0x55
	tailByte1  = 0xCC
)

// Target Data structure matching the Javascript payload configuration schema
type Target struct {
	ID    int     `json:"id"`
	X     int16   `json:"x"`
	Y     int16   `json:"y"`
	Speed int16   `json:"speed"`
	Dist  float64 `json:"dist"`
	Gate  uint16  `json:"gate"`
}

// Hub handles multiple browser connections simultaneously
type Hub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan []Target
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mu         sync.Mutex
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			originURL, err := url.Parse(origin)
			return err == nil && originURL.Host == r.Host
		},
	}
	hub = Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []Target, 100),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
)

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Close()
			}
			h.mu.Unlock()
		case targets := <-h.broadcast:
			h.mu.Lock()
			for client := range h.clients {
				_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
				err := client.WriteJSON(targets)
				if err != nil {
					client.Close()
					delete(h.clients, client)
				}
			}
			h.mu.Unlock()
		}
	}
}

func main() {
	alertDistance := flag.Float64("d", 0, "notify when a target is within this distance in meters")
	certFile := flag.String("cert", "", "TLS certificate file for HTTPS")
	keyFile := flag.String("key", "", "TLS private key file for HTTPS")
	flag.Parse()
	if *alertDistance < 0 {
		log.Fatal("distance must be zero or greater")
	}
	if (*certFile == "") != (*keyFile == "") {
		log.Fatal("-cert and -key must be provided together")
	}

	fmt.Println("======================================================")
	fmt.Println("       HLK-LD2450 REALTIME GRAPHICAL RADAR HUB        ")
	fmt.Println("======================================================")

	// 1. Discover COM / TTY hardware devices
	ports, err := serial.GetPortsList()
	if err != nil {
		log.Fatalf("❌ Failed to query system serial paths: %v", err)
	}

	if len(ports) == 0 {
		fmt.Println("❌ Error: No active serial ports detected on this computer.")
		fmt.Println("\nPress Enter to close this application...")
		fmt.Scanln()
		os.Exit(1)
	}

	fmt.Println("\nAvailable Serial Ports:")
	for i, p := range ports {
		fmt.Printf("  [%d] %s\n", i+1, p)
	}
	fmt.Println("  [X] Exit Program")
	fmt.Println("------------------------------------------------------")

	var choice string
	selectedIndex := -1
	for selectedIndex == -1 {
		fmt.Print("👉 Enter the menu number for your FTDI adapter: ")
		_, err := fmt.Scanln(&choice)
		if err != nil {
			continue
		}
		if choice == "X" || choice == "x" {
			os.Exit(0)
		}
		var val int
		_, err = fmt.Sscanf(choice, "%d", &val)
		if err == nil && val >= 1 && val <= len(ports) {
			selectedIndex = val - 1
		} else {
			fmt.Printf("❌ Range error. Enter a number between 1 and %d.\n", len(ports))
		}
	}

	portName := ports[selectedIndex]

	// 2. Open serial bus link
	mode := &serial.Mode{
		BaudRate: 256000,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(portName, mode)
	if err != nil {
		log.Fatalf("❌ Open error on %s: %v", portName, err)
	}
	defer port.Close()

	// 3. Fire up Web Server routines concurrently
	go hub.Run()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveHome(w, r, *alertDistance)
	})
	http.HandleFunc("/ws", serveWS)
	go func() {
		if *certFile != "" {
			fmt.Println("🌐 Web UI available at: https://localhost:8080")
			if err := http.ListenAndServeTLS(":8080", *certFile, *keyFile, nil); err != nil {
				log.Fatalf("❌ HTTPS Web Server failed to start: %v", err)
			}
			return
		}
		fmt.Println("🌐 Web UI available at: http://localhost:8080")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatalf("❌ Web Server failed to start: %v", err)
		}
	}()

	fmt.Println("✅ Data pipeline active. Listening for targets...")

	// 4. Dedicated stream loop parsing chunks out of the ring buffer window
	rawBuffer := make([]byte, 0, 4096)
	readBuf := make([]byte, 512)

	for {
		n, err := port.Read(readBuf)
		if err != nil {
			log.Printf("⚠️ Line disconnected: %v\n", err)
			break
		}
		if n > 0 {
			rawBuffer = append(rawBuffer, readBuf[:n]...)

			for len(rawBuffer) >= packetSize {
				if rawBuffer[0] != headByte0 || rawBuffer[1] != headByte1 {
					idx := bytes.Index(rawBuffer, []byte{headByte0, headByte1})
					if idx == -1 {
						rawBuffer = rawBuffer[len(rawBuffer):]
					} else {
						rawBuffer = rawBuffer[idx:]
					}
					continue
				}

				if len(rawBuffer) < packetSize {
					break
				}

				if rawBuffer[packetSize-2] != tailByte0 || rawBuffer[packetSize-1] != tailByte1 {
					rawBuffer = rawBuffer[1:]
					continue
				}

				packet := rawBuffer[:packetSize]
				rawBuffer = rawBuffer[packetSize:]

				// Process targets concurrently in memory frames
				currentFrame := make([]Target, 0, 3)
				if t1, active := parseSegment(1, packet[4:12]); active {
					currentFrame = append(currentFrame, t1)
				}
				if t2, active := parseSegment(2, packet[12:20]); active {
					currentFrame = append(currentFrame, t2)
				}
				if t3, active := parseSegment(3, packet[20:28]); active {
					currentFrame = append(currentFrame, t3)
				}

				// Ship data directly across WebSockets, including empty frames so the UI can clear stale targets.
				select {
				case hub.broadcast <- currentFrame:
				default: // Prevent slow web clients from locking up the serial streaming parser
				}
			}
		}
	}
}

func parseSegment(id int, data []byte) (Target, bool) {
	rawX := binary.LittleEndian.Uint16(data[0:2])
	rawY := binary.LittleEndian.Uint16(data[2:4])
	rawSpeed := binary.LittleEndian.Uint16(data[4:6])
	res := binary.LittleEndian.Uint16(data[6:8])

	if rawY == 0 {
		return Target{}, false
	}

	var x int16
	if (rawX & 0x8000) != 0 {
		x = -int16(rawX & 0x7FFF)
	} else {
		x = int16(rawX & 0x7FFF)
	}

	y := int16(rawY & 0x7FFF)

	var speed int16
	if (rawSpeed & 0x8000) != 0 {
		speed = -int16(rawSpeed & 0x7FFF)
	} else {
		speed = int16(rawSpeed & 0x7FFF)
	}

	return Target{
		ID:    id,
		X:     x,
		Y:     y,
		Speed: speed,
		Dist:  math.Hypot(float64(x), float64(y)) / 1000.0,
		Gate:  res,
	}, true
}

func serveHome(w http.ResponseWriter, r *http.Request, alertDistance float64) {
	verbose := r.URL.Query().Get("verbose") == "1"
	if verbose {
		fmt.Printf("[debug] HTTP page request: %s %s\n", r.Method, r.URL.String())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := radarPage
	page = strings.NewReplacer(
		"// Draw Text Metric Markersctx.fillStyle", "/* Draw Text Metric Markers */ ctx.fillStyle",
		"// Distance Arc Rings (Drawn at every 1-meter layer increment)for", "/* Distance Arc Rings (Drawn at every 1-meter layer increment) */ for",
		"// Layer metric text print out loopsctx.fillStyle", "/* Layer metric text print out loops */ ctx.fillStyle",
		"// Radar Device Module Center Badge Position Indicatorctx.fillStyle", "/* Radar Device Module Center Badge Position Indicator */ ctx.fillStyle",
		"// Run decay routines over old tracking history breadcrumbs[1,2,3].forEach", "/* Run decay routines over old tracking history */ [1,2,3].forEach",
		"// Slices old echoes down smoothly", "/* Slices old echoes down smoothly */",
		"// Iterate over pipeline packagestargets.forEach", "/* Iterate over pipeline packages */ targets.forEach",
		"// Cap breadcrumb array frame count sizes", "/* Cap breadcrumb array frame count sizes */",
		"// Render fading breadcrumb tracksfor", "/* Render fading breadcrumb tracks */ for",
		"// Reset canvas context state blending opacity modifiers", "/* Reset canvas context state blending opacity modifiers */",
		"// Paint target point location dot matrix vectorslet", "/* Paint target point location dot matrix vectors */ let",
		"// Draw a small crosshair vector ring around active target vectorsctx.strokeStyle", "/* Draw a small crosshair vector ring around active target vectors */ ctx.strokeStyle",
		"// Clean visual state adjustments back into raw execution scopesctx.shadowBlur", "/* Clean visual state adjustments back into raw execution scopes */ ctx.shadowBlur",
		"// Target identification indicator markup numbers text printing overlayctx.fillStyle", "/* Target identification indicator */ ctx.fillStyle",
		"// Establish connection to backend websocket instance network loop handler enginesconst", "/* Establish WebSocket connection */ const",
		"// Initialize empty baseline canvas framework configuration immediately upon file run parsing hooks execution context boundariesdrawRadarBase();", "/* Initialize canvas */ drawRadarBase();",
		"const ws = new WebSocket('ws://' + window.location.host + '/ws')", "const verbose = new URLSearchParams(window.location.search).get('verbose') === '1'; if (verbose) console.log('[radar] script loaded'); const ws = new WebSocket('ws://' + window.location.host + '/ws' + window.location.search)",
		"ws.onopen = () => {", "ws.onopen = () => { if (verbose) console.log('[radar] WebSocket opened');",
		"ws.onmessage = (event) => {", "ws.onmessage = (event) => { if (verbose) console.log('[radar] frame received', event.data);",
		"ws.onclose = () => {", "ws.onerror = (event) => { if (verbose) console.error('[radar] WebSocket error', event); }; ws.onclose = () => { if (verbose) console.warn('[radar] WebSocket closed');",
	).Replace(page)
	page = strings.ReplaceAll(page,
		"const ws = new WebSocket('ws://' + window.location.host + '/ws')",
		"const verbose = new URLSearchParams(window.location.search).get('verbose') === '1'; if (verbose) console.log('[radar] script loaded'); const ws = new WebSocket('ws://' + window.location.host + '/ws' + window.location.search)")
	tmpl, err := template.New("index").Parse(page)
	if err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		fmt.Printf("[debug] template parse error: %v\n", err)
		return
	}
	if err := tmpl.Execute(w, struct{ AlertDistance float64 }{AlertDistance: alertDistance}); err != nil {
		fmt.Printf("[debug] template execute error: %v\n", err)
	} else if verbose {
		fmt.Println("[debug] HTTP page rendered successfully")
	}
}

func serveWS(w http.ResponseWriter, r *http.Request) {
	verbose := r.URL.Query().Get("verbose") == "1"
	fmt.Printf("[debug] WebSocket request from %s, origin=%q, verbose=%t\n", r.RemoteAddr, r.Header.Get("Origin"), verbose)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("[debug] WebSocket upgrade failed: %v\n", err)
		return
	}
	fmt.Printf("[debug] WebSocket handshake succeeded: remote=%s protocol=%q\n", r.RemoteAddr, conn.Subprotocol())
	hub.register <- conn
	defer func() {
		fmt.Printf("[debug] WebSocket disconnected: %s\n", r.RemoteAddr)
		hub.unregister <- conn
	}()

	// Keep alive wait loop
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

const radarPage = `
<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>HLK-LD2450</title>
	<style>
		:root { color-scheme: dark; }
		* { box-sizing: border-box; }
		html, body { margin: 0; width: 100%; height: 100%; overflow: hidden; background: #0d1117; color: #c9d1d9; font-family: "Segoe UI", sans-serif; }
		body { padding: 14px 18px 18px; }
		header { position: absolute; z-index: 1; top: 14px; left: 18px; right: 18px; display: flex; flex-direction: column; align-items: flex-end; pointer-events: none; }
		#controls { display: flex; align-items: center; gap: 18px; pointer-events: auto; }
		#status { display: inline-flex; align-items: center; gap: 8px; color: #8b949e; font-size: 13px; }
		#status::before { content: ""; width: 9px; height: 9px; border-radius: 50%; background: #f85149; box-shadow: 0 0 8px #f85149; }
		#status.connected { color: #55ff55; }
		#status.connected::before { background: #55ff55; box-shadow: 0 0 8px #55ff55; }
		#alert { display: none; flex-direction: column; align-items: flex-end; gap: 4px; margin-top: 8px; color: #ffaaa5; font: normal 13px "Segoe UI", sans-serif; white-space: nowrap; }
		#alert.active { display: flex; }
		.zone-alert { display: inline-flex; align-items: center; gap: 8px; }
		.zone-alert::before { content: ""; width: 9px; height: 9px; border-radius: 50%; background: #f85149; box-shadow: 0 0 8px #f85149; animation: alert-blink 0.8s steps(1, end) infinite; }
		@keyframes alert-blink { 50% { opacity: 0.2; } }
		#shoot-toggle { display: inline-flex; align-items: center; gap: 7px; color: #8b949e; font-size: 13px; cursor: pointer; }
		#shoot-toggle input { accent-color: #58a6ff; }
		.canvas-container { position: relative; width: 100%; max-width: 1600px; height: calc(100dvh - 32px); margin: 0 auto; border: 1px solid #30363d; background: #161b22; overflow: hidden; }
		canvas { display: block; width: 100%; height: 100%; }
	</style>
</head>
<body>
	<main class="canvas-container"><header><div id="controls"><label id="shoot-toggle"><input id="shoot-enabled" type="checkbox"> Shoot</label><div id="status">Disconnected</div></div><div id="alert" role="status" aria-live="assertive"></div></header><canvas id="radarCanvas"></canvas></main>
	<script>
		const canvas = document.getElementById("radarCanvas");
		const ctx = canvas.getContext("2d");
		const statusDiv = document.getElementById("status");
		const shootEnabledInput = document.getElementById("shoot-enabled");
		const alertDiv = document.getElementById("alert");
		const alertDistance = {{.AlertDistance}};
		const maxRange = 6000;
		const maxAngle = 60 * Math.PI / 180;
		const colors = { 1: "#ff5555", 2: "#55ff55", 3: "#5555ff" };
		let originX, originY, scale;
		let targetHistory = { 1: [], 2: [], 3: [] };
		let currentTargets = [];

		function resizeCanvas() {
			const ratio = window.devicePixelRatio || 1;
			const width = canvas.parentElement.clientWidth;
			const height = canvas.parentElement.clientHeight;
			canvas.width = Math.round(width * ratio);
			canvas.height = Math.round(height * ratio);
			ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
			originX = width / 2;
			const portrait = height > width;
			scale = Math.min(
				(portrait ? (height - 92) / (1.5 * maxRange) : (height - 54) / maxRange),
				(width - 32) / (2 * Math.sin(maxAngle) * maxRange)
			);
			const radarRadius = maxRange * scale;
			originY = portrait ? height / 2 + radarRadius / 2 : height - 32;
			renderFrame(currentTargets);
		}

		function drawRadarBase() {
			const width = canvas.clientWidth;
			const height = canvas.parentElement.clientHeight;
			ctx.clearRect(0, 0, width, height);
			ctx.fillStyle = "#58a6ff"; ctx.font = "400 20px Segoe UI, sans-serif"; ctx.textAlign = "left"; ctx.textBaseline = "top";
			ctx.fillText("HLK-LD2450", 18, 16);
			ctx.fillStyle = "rgba(22, 33, 45, 0.4)";
			ctx.beginPath(); ctx.moveTo(originX, originY);
			ctx.arc(originX, originY, maxRange * scale, -Math.PI / 2 - maxAngle, -Math.PI / 2 + maxAngle); ctx.closePath(); ctx.fill();
			if (alertDistance > 0) { const alertRadius = alertDistance * 1000 * scale; ctx.fillStyle = "rgba(248, 81, 73, 0.12)"; ctx.beginPath(); ctx.moveTo(originX, originY); ctx.arc(originX, originY, alertRadius, -Math.PI / 2 - maxAngle, -Math.PI / 2 + maxAngle); ctx.closePath(); ctx.fill(); }
			ctx.strokeStyle = "#30363d"; ctx.lineWidth = 1; ctx.fillStyle = "#8b949e"; ctx.font = "10px monospace"; ctx.textAlign = "center"; ctx.textBaseline = "middle";
			[-60, -45, -30, -15, 0, 15, 30, 45, 60].forEach(deg => { const rad = (deg - 90) * Math.PI / 180; const endX = originX + Math.cos(rad) * maxRange * scale, endY = originY + Math.sin(rad) * maxRange * scale; ctx.beginPath(); ctx.moveTo(originX, originY); ctx.lineTo(endX, endY); ctx.stroke(); ctx.fillText((deg + 90) + "°", originX + Math.cos(rad) * (maxRange * scale - 16), originY + Math.sin(rad) * (maxRange * scale - 16)); });
			for (let meter = 1; meter <= 6; meter++) { const radius = meter * 1000 * scale; ctx.strokeStyle = meter === 6 ? "#58a6ff" : "#30363d"; ctx.beginPath(); ctx.arc(originX, originY, radius, -Math.PI / 2 - maxAngle, -Math.PI / 2 + maxAngle); ctx.stroke(); ctx.fillStyle = "#8b949e"; ctx.font = "10px monospace"; ctx.fillText(meter + "m", originX + 10, originY - radius + 12); }
			ctx.fillStyle = "#58a6ff"; ctx.beginPath(); ctx.arc(originX, originY, 5, 0, 2 * Math.PI); ctx.fill();
		}

		function drawLegend(targets, height) {
			const entries = targets.map(target => ({ target, width: ctx.measureText("(" + target.x + "," + target.y + ")").width + 34 }));
			let x = 16; const y = height - 14;
			ctx.font = "12px monospace"; ctx.textAlign = "left"; ctx.textBaseline = "middle";
			entries.forEach(({ target }) => { ctx.fillStyle = colors[target.id]; ctx.beginPath(); ctx.arc(x + 5, y, 5, 0, 2 * Math.PI); ctx.fill(); ctx.fillStyle = "#c9d1d9"; ctx.fillText("(" + target.x + "," + target.y + ")", x + 16, y); x += ctx.measureText("(" + target.x + "," + target.y + ")").width + 36; });
		}

		function drawShots(alertTargets, timestamp) {
			alertTargets.forEach(target => {
				const targetX = originX + target.x * scale, targetY = originY - target.y * scale;
				const progress = ((timestamp + target.id * 180) % 900) / 450;
				if (progress <= 1) {
					const bulletX = originX + (targetX - originX) * progress, bulletY = originY + (targetY - originY) * progress;
					ctx.globalAlpha = 1; ctx.fillStyle = "#ff3b30"; ctx.shadowColor = "#ff3b30"; ctx.shadowBlur = 10; ctx.beginPath(); ctx.arc(bulletX, bulletY, 4, 0, 2 * Math.PI); ctx.fill(); ctx.shadowBlur = 0;
				}
				const firing = progress <= 0.12;
				if (firing) { ctx.globalAlpha = 1; ctx.fillStyle = "#58a6ff"; ctx.shadowColor = "#58a6ff"; ctx.shadowBlur = 16; ctx.beginPath(); ctx.arc(originX, originY, 7 + progress * 18, 0, 2 * Math.PI); ctx.fill(); ctx.shadowBlur = 0; }
			});
		}

		function drawShootingLabel(target) {
			if (!target) return;
			const shootingAngle = Math.atan2(target.x, target.y) * 180 / Math.PI;
			ctx.globalAlpha = 1; ctx.fillStyle = "#58a6ff"; ctx.font = "12px monospace"; ctx.textAlign = "left"; ctx.textBaseline = "middle";
			ctx.fillText("Shoot " + shootingAngle.toFixed(0) + "°", originX + 14, originY);
		}

		function renderFrame(targets, updateTracking = true) {
			if (updateTracking) currentTargets = targets;
			drawRadarBase();
			const alertTargets = alertDistance > 0 ? targets.filter(target => target.dist <= alertDistance) : [];
			const shootingTarget = alertTargets.reduce((nearest, target) => !nearest || target.dist < nearest.dist ? target : nearest, null);
			alertDiv.innerHTML = alertTargets.map(target => '<div class="zone-alert">Target ' + target.id + ' on red zone</div>').join("");
			alertDiv.classList.toggle("active", alertTargets.length > 0);
			if (updateTracking) [1, 2, 3].forEach(id => { const history = targetHistory[id]; if (!targets.some(target => target.id === id) && history.length) history.shift(); });
			targets.forEach(target => {
				const history = targetHistory[target.id]; if (updateTracking) { history.push({ x: target.x, y: target.y }); if (history.length > 15) history.shift(); }
				history.forEach((point, index) => { ctx.globalAlpha = (index / history.length) * 0.4; ctx.fillStyle = colors[target.id]; ctx.beginPath(); ctx.arc(originX + point.x * scale, originY - point.y * scale, 3, 0, 2 * Math.PI); ctx.fill(); });
				const x = originX + target.x * scale, y = originY - target.y * scale;
				ctx.globalAlpha = 1; ctx.fillStyle = colors[target.id]; ctx.shadowColor = colors[target.id]; ctx.shadowBlur = 10; ctx.beginPath(); ctx.arc(x, y, 8, 0, 2 * Math.PI); ctx.fill(); ctx.shadowBlur = 0;
				ctx.strokeStyle = "#fff"; ctx.lineWidth = 1.5; ctx.beginPath(); ctx.arc(x, y, 12, 0, 2 * Math.PI); ctx.stroke();
				ctx.fillStyle = "#000"; ctx.font = "bold 10px monospace"; ctx.textAlign = "center"; ctx.textBaseline = "middle"; ctx.fillText(target.id, x, y);
				ctx.fillStyle = "#c9d1d9"; ctx.font = "12px monospace"; ctx.textAlign = target.x < 0 ? "right" : "left"; ctx.fillText(target.dist.toFixed(2) + "m", x + (target.x < 0 ? -16 : 16), y - 14); ctx.fillText(target.speed + "mm/s", x + (target.x < 0 ? -16 : 16), y + 1);
			});
			if (shootEnabledInput.checked) { drawShots(alertTargets, performance.now()); drawShootingLabel(shootingTarget); }
			drawLegend(targets, canvas.parentElement.clientHeight);
		}

		function animate() { renderFrame(currentTargets, false); requestAnimationFrame(animate); }

		window.addEventListener("resize", resizeCanvas);
		resizeCanvas();
		requestAnimationFrame(animate);
		const webSocketProtocol = window.location.protocol === "https:" ? "wss://" : "ws://";
		const ws = new WebSocket(webSocketProtocol + window.location.host + "/ws" + window.location.search);
		ws.onopen = function () { statusDiv.textContent = "Connected"; statusDiv.classList.add("connected"); };
		ws.onmessage = event => renderFrame(JSON.parse(event.data));
		ws.onclose = function () { statusDiv.textContent = "Disconnected"; statusDiv.classList.remove("connected"); };
		ws.onerror = function () { statusDiv.textContent = "Disconnected"; statusDiv.classList.remove("connected"); };
	</script>
</body>
</html>
`

// 5. Embedded Frontend User Interface Template with HTML5 Canvas Grid Painting engine
const htmlTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>Hi-Link HLK-LD2450 Radar Visualiser</title>
    <style>
        body {
            background-color: #0d1117;
            color: #c9d1d9;
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            margin: 0;
            padding: 20px;
            display: flex;
            flex-direction: column;
            align-items: center;
        }
        h1 { color: #58a6ff; margin-bottom: 5px; font-weight: 400;}
        #status { font-size: 14px; margin-bottom: 20px; color: #8b949e; }
        .canvas-container {
            background-color: #161b22;
            border: 1px solid #30363d;
            border-radius: 8px;
            padding: 15px;
            box-shadow: 0 4px 12px rgba(0,0,0,0.5);
        }
        canvas { display: block; }
        #telemetry-box {
            margin-top: 20px;
            width: 700px;
            display: grid;
            grid-template-columns: repeat(3, 1fr);
            gap: 15px;
        }
        .card {
            background: #21262d;
            border: 1px solid #30363d;
            border-radius: 6px;
            padding: 10px;
            font-family: monospace;
        }
        .card h3 { margin: 0 0 8px 0; font-size: 14px; }
        .t1 { border-top: 3px solid #ff5555; }
        .t2 { border-top: 3px solid #55ff55; }
        .t3 { border-top: 3px solid #5555ff; }
        .inactive { opacity: 0.25; border-top: 3px solid #484f58; }
    </style>
</head>
<body>

    <h1>HLK-LD2450</h1>
    <div id="status">Connecting to Go Engine Web Socket...</div>

    <div class="canvas-container">
        <canvas id="radarCanvas" width="700" height="550"></canvas>
    </div>

    <div id="telemetry-box">
        <div id="card-1" class="card t1 inactive"><h3>Target 1</h3><div>Offline</div></div>
        <div id="card-2" class="card t2 inactive"><h3>Target 2</h3><div>Offline</div></div>
        <div id="card-3" class="card t3 inactive"><h3>Target 3</h3><div>Offline</div></div>
    </div>

    <script>
        const canvas = document.getElementById('radarCanvas');
        const ctx = canvas.getContext('2d');
        const statusDiv = document.getElementById('status');
        
        // Configuration Parameters matching HLK tool specifications
        const maxRange = 6000; // 6 meters max visual tracking reach
        const maxAngle = 60 * Math.PI / 180; // 120-degree composite sector cone
        
        // Dynamic scaling functions mapping real world coordinates to canvas pixels
        // Origin point placed bottom middle of viewport matching real hardware sensor lens projection
        const originX = canvas.width / 2;
        const originY = canvas.height - 40;
        const scale = (canvas.height - 80) / maxRange;

        // Static target tracks persistent cache for breadcrumb tail traces
        let targetHistory = { 1: [], 2: [], 3: [] };
        const colors = { 1: '#ff5555', 2: '#55ff55', 3: '#5555ff' };

        function drawRadarBase() {
            ctx.clearRect(0, 0, canvas.width, canvas.height);

            // Draw Sector Field of View fill bounding box
            ctx.fillStyle = 'rgba(22, 33, 45, 0.4)';
            ctx.beginPath();
            ctx.moveTo(originX, originY);
            ctx.arc(originX, originY, maxRange * scale, -Math.PI/2 - maxAngle, -Math.PI/2 + maxAngle);
            ctx.closePath();
            ctx.fill();

            // Angular Grid Guideline Framework
          ctx.strokeStyle = '#21262d';ctx.lineWidth = 1;const targetAngles = [-60, -30, 0, 30];targetAngles.forEach(deg => {let rad = (deg - 90) * Math.PI / 180;ctx.beginPath();ctx.moveTo(originX, originY);ctx.lineTo(originX + Math.cos(rad) * maxRange * scale, originY + Math.sin(rad) * maxRange * scale);ctx.stroke();// Draw Text Metric Markersctx.fillStyle = '#8b949e';ctx.font = '10px monospace';let tx = originX + Math.cos(rad) * (maxRange * scale + 15);let ty = originY + Math.sin(rad) * (maxRange * scale + 15);ctx.textAlign = 'center';ctx.fillText(deg + '°', tx, ty);});// Distance Arc Rings (Drawn at every 1-meter layer increment)for(let m = 1; m <= 6; m++) {let r = m * 1000 * scale;ctx.strokeStyle = m === 6 ? '#58a6ff' : '#30363d';ctx.lineWidth = m === 6 ? 1.5 : 1;ctx.beginPath();ctx.arc(originX, originY, r, -Math.PI/2 - maxAngle, -Math.PI/2 + maxAngle);ctx.stroke();// Layer metric text print out loopsctx.fillStyle = '#8b949e';ctx.fillText(m + 'm', originX + 15, originY - r + 12);}// Radar Device Module Center Badge Position Indicatorctx.fillStyle = '#58a6ff';ctx.beginPath();ctx.arc(originX, originY, 6, 0, 2*Math.PI);ctx.fill();ctx.fillStyle = '#ffffff';ctx.font = 'bold 9px Arial';ctx.fillText("RADAR", originX, originY + 18);}function updateTelemetryCards(targets) {[1, 2, 3].forEach(id => {const card = document.getElementById('card-' + id);const target = targets.find(t => t.id === id);if (target) {card.classList.remove('inactive');card.querySelector('div').innerHTML = X: ${target.x}mm<br> Y: ${target.y}mm<br> Dist: ${target.dist.toFixed(2)}m<br> Speed: ${target.speed}mm/s<br> Gate: ${target.gate};} else {card.classList.add('inactive');card.querySelector('div').innerHTML = 'Offline';}});}function renderFrame(targets) {drawRadarBase();// Run decay routines over old tracking history breadcrumbs[1,2,3].forEach(id => {let history = targetHistory[id];if (!targets.some(t => t.id === id)) {if (history.length > 0) history.shift(); // Slices old echoes down smoothly}});// Iterate over pipeline packagestargets.forEach(t => {let history = targetHistory[t.id];history.push({x: t.x, y: t.y});if (history.length > 15) history.shift(); // Cap breadcrumb array frame count sizes// Render fading breadcrumb tracksfor(let i = 0; i < history.length - 1; i++) {let pt = history[i];let cx = originX + (pt.x * scale);let cy = originY - (pt.y * scale);ctx.fillStyle = colors[t.id];ctx.globalAlpha = (i / history.length) * 0.4;ctx.beginPath();ctx.arc(cx, cy, 3, 0, 2 * Math.PI);ctx.fill();}ctx.globalAlpha = 1.0; // Reset canvas context state blending opacity modifiers// Paint target point location dot matrix vectorslet cx = originX + (t.x * scale);let cy = originY - (t.y * scale);ctx.fillStyle = colors[t.id];ctx.shadowColor = colors[t.id];ctx.shadowBlur = 10;ctx.beginPath();ctx.arc(cx, cy, 8, 0, 2 * Math.PI);ctx.fill();// Draw a small crosshair vector ring around active target vectorsctx.strokeStyle = '#ffffff';ctx.lineWidth = 1.5;ctx.beginPath();ctx.arc(cx, cy, 12, 0, 2 * Math.PI);ctx.stroke();// Clean visual state adjustments back into raw execution scopesctx.shadowBlur = 0;// Target identification indicator markup numbers text printing overlayctx.fillStyle = '#000000';ctx.font = 'bold 10px monospace';ctx.textAlign = 'center';ctx.textBaseline = 'middle';ctx.fillText(t.id, cx, cy);});}// Establish connection to backend websocket instance network loop handler enginesconst ws = new WebSocket('ws://' + window.location.host + '/ws');ws.onopen = () => {statusDiv.innerHTML = "⚡ WebSocket Connection Active | Streaming Telemetry Frames";statusDiv.style.color = "#55ff55";};ws.onmessage = (event) => {const targets = JSON.parse(event.data);renderFrame(targets);updateTelemetryCards(targets);};ws.onclose = () => {statusDiv.innerHTML = "❌ WebSocket Disconnected from Go backend layer module engine framework";statusDiv.style.color = "#ff5555";};// Initialize empty baseline canvas framework configuration immediately upon file run parsing hooks execution context boundariesdrawRadarBase();`
