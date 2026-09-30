/*
  HLK-LD2450 realtime radar web UI for Waveshare ESP32-C3-Zero

  Wiring (LD2450 -> ESP32-C3-Zero):
    5V  -> 5V
    GND -> GND
    TX  -> GPIO4  (ESP RX)
    RX  -> GPIO5  (ESP TX)

  Libraries (Library Manager):
    - Adafruit NeoPixel
    - WebSockets by Markus Sattler (Links2004 / arduinoWebSockets)

  Board settings: ESP32C3 Dev Module, "USB CDC On Boot: Enabled"
  (so Serial prints appear over the USB port).

  Web UI:  http://<ip>/        WebSocket: ws://<ip>:81/
*/

#include <WiFi.h>
#include <WebServer.h>
#include <WebSocketsServer.h>
#include <Adafruit_NeoPixel.h>

// ---------------- USER CONFIG ----------------
const char* WIFI_SSID = "<YOUR WIFI SSID>";
const char* WIFI_PASS = "<YOUR WIFI PASSWORD>";

// Fallback access point, used if the WiFi above cannot be joined
const char* AP_SSID = "LD2450-Radar";
const char* AP_PASS = "radar1234";        // at least 8 characters, or "" for an open network
const unsigned long WIFI_TIMEOUT_MS = 15000;  // how long to try joining WiFi before falling back to AP

const float ALERT_DISTANCE_M = 1.5;  // red zone radius in meters (0 = disabled)

#define LD_RX_PIN 20   // ESP32 RX <- LD2450 TX
#define LD_TX_PIN 21   // ESP32 TX -> LD2450 RX
#define LD_BAUD   256000

#define PIN_RGB   10       // Onboard WS2812
#define NUMPIXELS 1

// Set to 1 if left/right or approaching/receding appear mirrored on your unit
#define FLIP_X     0
#define FLIP_SPEED 0
// ---------------------------------------------

Adafruit_NeoPixel pixels(NUMPIXELS, PIN_RGB, NEO_GRB + NEO_KHZ800);
WebServer server(80);
WebSocketsServer webSocket(81);

constexpr uint8_t PACKET_SIZE = 30;

struct Target {
  uint8_t id;
  int16_t x, y, speed;
  float dist;
  uint16_t gate;
};

uint8_t packetBuf[PACKET_SIZE];
uint8_t packetIdx = 0;

bool alertActive = false;
bool apMode = false;
unsigned long lastFrameMs = 0;

// ---------------- Web page ----------------
const char RADAR_PAGE[] PROGMEM = R"rawliteral(
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
const alertDistance = %ALERT%;
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
  [-60, -45, -30, -15, 0, 15, 30, 45, 60].forEach(deg => { const rad = (deg - 90) * Math.PI / 180; const endX = originX + Math.cos(rad) * maxRange * scale, endY = originY + Math.sin(rad) * maxRange * scale; ctx.beginPath(); ctx.moveTo(originX, originY); ctx.lineTo(endX, endY); ctx.stroke(); ctx.fillText((deg + 90) + "\u00B0", originX + Math.cos(rad) * (maxRange * scale - 16), originY + Math.sin(rad) * (maxRange * scale - 16)); });
  for (let meter = 1; meter <= 6; meter++) { const radius = meter * 1000 * scale; ctx.strokeStyle = meter === 6 ? "#58a6ff" : "#30363d"; ctx.beginPath(); ctx.arc(originX, originY, radius, -Math.PI / 2 - maxAngle, -Math.PI / 2 + maxAngle); ctx.stroke(); ctx.fillStyle = "#8b949e"; ctx.font = "10px monospace"; ctx.fillText(meter + "m", originX + 10, originY - radius + 12); }
  ctx.fillStyle = "#58a6ff"; ctx.beginPath(); ctx.arc(originX, originY, 5, 0, 2 * Math.PI); ctx.fill();
}

function drawLegend(targets, height) {
  let x = 16; const y = height - 14;
  ctx.font = "12px monospace"; ctx.textAlign = "left"; ctx.textBaseline = "middle";
  targets.forEach(target => { ctx.fillStyle = colors[target.id]; ctx.beginPath(); ctx.arc(x + 5, y, 5, 0, 2 * Math.PI); ctx.fill(); ctx.fillStyle = "#c9d1d9"; const label = "(" + target.x + "," + target.y + ")"; ctx.fillText(label, x + 16, y); x += ctx.measureText(label).width + 36; });
}

function drawShots(alertTargets, timestamp) {
  alertTargets.forEach(target => {
    const targetX = originX + target.x * scale, targetY = originY - target.y * scale;
    const progress = ((timestamp + target.id * 180) % 900) / 450;
    if (progress <= 1) {
      const bulletX = originX + (targetX - originX) * progress, bulletY = originY + (targetY - originY) * progress;
      ctx.globalAlpha = 1; ctx.fillStyle = "#ff3b30"; ctx.shadowColor = "#ff3b30"; ctx.shadowBlur = 10; ctx.beginPath(); ctx.arc(bulletX, bulletY, 4, 0, 2 * Math.PI); ctx.fill(); ctx.shadowBlur = 0;
    }
    if (progress <= 0.12) { ctx.globalAlpha = 1; ctx.fillStyle = "#58a6ff"; ctx.shadowColor = "#58a6ff"; ctx.shadowBlur = 16; ctx.beginPath(); ctx.arc(originX, originY, 7 + progress * 18, 0, 2 * Math.PI); ctx.fill(); ctx.shadowBlur = 0; }
  });
}

function drawShootingLabel(target) {
  if (!target) return;
  const shootingAngle = Math.atan2(target.x, target.y) * 180 / Math.PI;
  ctx.globalAlpha = 1; ctx.fillStyle = "#58a6ff"; ctx.font = "12px monospace"; ctx.textAlign = "left"; ctx.textBaseline = "middle";
  ctx.fillText("Shoot " + shootingAngle.toFixed(0) + "\u00B0", originX + 14, originY);
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
  ctx.globalAlpha = 1;
  if (shootEnabledInput.checked) { drawShots(alertTargets, performance.now()); drawShootingLabel(shootingTarget); }
  drawLegend(targets, canvas.parentElement.clientHeight);
}

function animate() { renderFrame(currentTargets, false); requestAnimationFrame(animate); }

window.addEventListener("resize", resizeCanvas);
resizeCanvas();
requestAnimationFrame(animate);

let ws;
function connect() {
  ws = new WebSocket("ws://" + window.location.hostname + ":81/");
  ws.onopen = function () { statusDiv.textContent = "Connected"; statusDiv.classList.add("connected"); };
  ws.onmessage = event => renderFrame(JSON.parse(event.data));
  ws.onclose = function () { statusDiv.textContent = "Disconnected"; statusDiv.classList.remove("connected"); setTimeout(connect, 2000); };
  ws.onerror = function () { ws.close(); };
}
connect();
</script>
</body>
</html>
)rawliteral";

// ---------------- LED ----------------
void setLed(uint8_t r, uint8_t g, uint8_t b) {
  pixels.setPixelColor(0, pixels.Color(r, g, b));
  pixels.show();
}

void updateLed() {
  static unsigned long lastToggle = 0;
  static bool blinkOn = false;

  // No frames for a while -> treat as nobody around
  bool active = alertActive && (millis() - lastFrameMs < 1000);

  if (!active) {
    static bool wasActive = true;
    if (wasActive || blinkOn) {
      setLed(0, 40, 0);          // steady green
      blinkOn = false;
      wasActive = false;
    }
    return;
  }

  if (millis() - lastToggle >= 250) {
    lastToggle = millis();
    blinkOn = !blinkOn;
    if (blinkOn) setLed(255, 0, 0);
    else setLed(0, 0, 0);
  }
}

// ---------------- Radar parsing ----------------
bool parseSegment(uint8_t id, const uint8_t* d, Target& t) {
  uint16_t rawX = d[0] | (d[1] << 8);
  uint16_t rawY = d[2] | (d[3] << 8);
  uint16_t rawS = d[4] | (d[5] << 8);
  uint16_t res  = d[6] | (d[7] << 8);

  if (rawX == 0 && rawY == 0 && rawS == 0 && res == 0) return false;  // empty slot

  // LD2450 encoding: MSB set = positive, MSB clear = negative
  int16_t x = (rawX & 0x8000) ? (int16_t)(rawX & 0x7FFF) : -(int16_t)(rawX & 0x7FFF);
  int16_t y = (int16_t)(rawY & 0x7FFF);
  int16_t s = (rawS & 0x8000) ? (int16_t)(rawS & 0x7FFF) : -(int16_t)(rawS & 0x7FFF);

#if FLIP_X
  x = -x;
#endif
#if FLIP_SPEED
  s = -s;
#endif

  t.id = id;
  t.x = x;
  t.y = y;
  t.speed = s;
  t.dist = sqrtf((float)x * x + (float)y * y) / 1000.0f;
  t.gate = res;
  return true;
}

void handlePacket(const uint8_t* p) {
  Target targets[3];
  uint8_t n = 0;
  bool inZone = false;

  for (uint8_t i = 0; i < 3; i++) {
    Target t;
    if (parseSegment(i + 1, p + 4 + i * 8, t)) {
      targets[n++] = t;
      if (ALERT_DISTANCE_M > 0 && t.dist <= ALERT_DISTANCE_M) inZone = true;
    }
  }

  alertActive = inZone;
  lastFrameMs = millis();

  if (webSocket.connectedClients() == 0) return;

  // Build JSON: [{"id":1,"x":..,"y":..,"speed":..,"dist":..,"gate":..}, ...]
  String json;
  json.reserve(200);
  json += '[';
  for (uint8_t i = 0; i < n; i++) {
    if (i) json += ',';
    json += "{\"id\":";    json += targets[i].id;
    json += ",\"x\":";     json += targets[i].x;
    json += ",\"y\":";     json += targets[i].y;
    json += ",\"speed\":"; json += targets[i].speed;
    json += ",\"dist\":";  json += String(targets[i].dist, 3);
    json += ",\"gate\":";  json += targets[i].gate;
    json += '}';
  }
  json += ']';
  webSocket.broadcastTXT(json);  // empty frames "[]" are sent too, so the UI clears stale targets
}

void readRadar() {
  while (Serial1.available()) {
    uint8_t b = Serial1.read();

    if (packetIdx == 0) {
      if (b == 0xAA) packetBuf[packetIdx++] = b;
      continue;
    }
    if (packetIdx == 1) {
      if (b == 0xFF) packetBuf[packetIdx++] = b;
      else packetIdx = (b == 0xAA) ? (packetBuf[0] = b, 1) : 0;
      continue;
    }

    packetBuf[packetIdx++] = b;

    if (packetIdx == PACKET_SIZE) {
      if (packetBuf[28] == 0x55 && packetBuf[29] == 0xCC) {
        handlePacket(packetBuf);
        packetIdx = 0;
      } else {
        // Bad tail: resync by looking for another header inside this buffer
        uint8_t next = 0;
        for (uint8_t i = 1; i < PACKET_SIZE - 1; i++) {
          if (packetBuf[i] == 0xAA && packetBuf[i + 1] == 0xFF) { next = i; break; }
        }
        if (next) {
          memmove(packetBuf, packetBuf + next, PACKET_SIZE - next);
          packetIdx = PACKET_SIZE - next;
        } else {
          packetIdx = 0;
        }
      }
    }
  }
}

// ---------------- Web ----------------
void handleRoot() {
  String page = FPSTR(RADAR_PAGE);
  page.replace("%ALERT%", String(ALERT_DISTANCE_M, 2));
  server.send(200, "text/html; charset=utf-8", page);
}

void onWsEvent(uint8_t num, WStype_t type, uint8_t* payload, size_t length) {
  if (type == WStype_CONNECTED) {
    Serial.printf("WebSocket client %u connected\n", num);
  } else if (type == WStype_DISCONNECTED) {
    Serial.printf("WebSocket client %u disconnected\n", num);
  }
}

void startAP() {
  Serial.println("Could not join WiFi. Starting access point...");
  WiFi.disconnect(true);
  WiFi.mode(WIFI_AP);
  bool ok = (strlen(AP_PASS) >= 8) ? WiFi.softAP(AP_SSID, AP_PASS) : WiFi.softAP(AP_SSID);
  apMode = true;
  if (!ok) Serial.println("Failed to start access point!");
  Serial.printf("Access point: %s", AP_SSID);
  if (strlen(AP_PASS) >= 8) Serial.printf("  password: %s", AP_PASS);
  Serial.println();
  Serial.print("IP address: ");
  Serial.println(WiFi.softAPIP());
  Serial.print("Web UI: http://");
  Serial.println(WiFi.softAPIP());
  setLed(0, 40, 0);
}

// Returns true if connected within WIFI_TIMEOUT_MS
bool connectWiFi() {
  apMode = false;
  WiFi.mode(WIFI_STA);
  WiFi.begin(WIFI_SSID, WIFI_PASS);
  Serial.printf("Connecting to %s", WIFI_SSID);
  unsigned long start = millis();
  bool on = false;
  while (WiFi.status() != WL_CONNECTED) {
    if (millis() - start > WIFI_TIMEOUT_MS) {
      Serial.println();
      return false;
    }
    delay(300);
    Serial.print('.');
    on = !on;
    setLed(0, 0, on ? 60 : 0);  // blue blink while connecting
  }
  Serial.println();
  Serial.print("Connected! IP address: ");
  Serial.println(WiFi.localIP());
  Serial.print("Web UI: http://");
  Serial.println(WiFi.localIP());
  return true;
}

void setup() {
  Serial.begin(115200);
  delay(1500);  // give USB CDC time to enumerate

  pixels.begin();
  pixels.setBrightness(80);
  setLed(0, 0, 0);

  Serial1.begin(LD_BAUD, SERIAL_8N1, LD_RX_PIN, LD_TX_PIN);

  if (!connectWiFi()) startAP();

  server.on("/", handleRoot);
  server.begin();
  webSocket.begin();
  webSocket.onEvent(onWsEvent);

  setLed(0, 40, 0);  // green: ready, nobody in zone
  Serial.println("Data pipeline active. Listening for targets...");
}

void loop() {
  if (!apMode && WiFi.status() != WL_CONNECTED) {
    Serial.println("WiFi lost, reconnecting...");
    if (!connectWiFi()) startAP();
  }
  server.handleClient();
  webSocket.loop();
  readRadar();
  updateLed();
}
