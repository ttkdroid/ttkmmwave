# HLK-LD2450 Real-Time Radar Web UI

This project turns a HLK-LD2450 mmWave radar sensor into a small live tracking system running on an ESP32 board. The radar sends target data over UART, the ESP32 parses it, and the result is shown in a browser-based radar display over Wi-Fi.

It also includes a red-zone alert mode, a live WebSocket stream, and a small RGB LED that shows whether a target is currently active.

## Overview

The system works like this:

1. The HLK-LD2450 radar scans the area and reports up to three tracked targets.
2. The ESP32 reads the UART packets from the radar.
3. The firmware decodes target position, speed, distance, and gate information.
4. The data is sent to a browser over WebSocket.
5. A web page draws the radar field and live target markers in real time.
6. If a target enters the configured alert radius, the UI and LED indicate the alarm condition.

## Hardware

### Main components

- HLK-LD2450 mmWave radar module
- ESP32-C3 or ESP32-C3-Zero compatible board
- Optional USB serial connection for programming/monitoring
- One WS2812 RGB LED (built into the board for status indication)
- Wi-Fi network or fallback access point

### Supported board and firmware pattern

This sketch is written for the ESP32 family and is configured for:

- Board: ESP32C3 Dev Module
- USB CDC: enabled
- Serial1 UART used for the LD2450

The code uses:

- UART1 RX on GPIO 20
- UART1 TX on GPIO 21

## Wiring

The project expects the following connections between the LD2450 and the ESP32:

| LD2450 pin | ESP32 pin | Notes |
| --- | --- | --- |
| 5V | 5V | Power |
| GND | GND | Common ground |
| TX | GPIO20 | ESP32 RX input |
| RX | GPIO21 | ESP32 TX output |

Important:

- The radar module uses TTL UART, so the TX from the radar connects to the ESP32 RX pin.
- The radar RX connects to the ESP32 TX pin.
- Make sure the grounds are common; otherwise the UART signal may not be valid.

If your LD2450 board exposes labels differently, verify the signal direction before wiring.

## Radar protocol and data flow

The firmware reads a fixed 30-byte packet from the LD2450. Each packet contains target data for up to three tracked objects.

The expected packet structure is:

- Header: `0xAA 0xFF`
- 3 target slots
- Footer: `0x55 0xCC`

The code then:

- Parses each target slot with `parseSegment()`
- Converts X/Y coordinates and speed into usable values
- Calculates each target's distance from the origin with the Pythagorean formula
- Marks the target as active if it is valid
- Builds a JSON array of all active targets
- Broadcasts the array over a WebSocket to the browser UI

The actual parsing logic is in the sketch, and the browser side receives arrays like:

```json
[
  {"id":1,"x":123,"y":456,"speed":120,"dist":0.72,"gate":15},
  {"id":2,"x":-220,"y":390,"speed":-30,"dist":1.16,"gate":21}
]
```

## Web interface

The browser page is embedded directly into the Arduino sketch as a large HTML string. It creates a real-time radar display with:

- a live radar sweep sector
- target markers
- distance labels
- motion history traces
- red-zone alert overlay
- simple "Shoot" simulation toggle

The page connects to:

- `http://<esp32-ip>/` for the browser UI
- `ws://<esp32-ip>:81/` for WebSocket streaming

The sketch also has a built-in access point fallback:

- SSID: `LD2450-Radar`
- Password: `radar1234`

If the configured Wi-Fi credentials fail, the board automatically starts the access point so you can still connect.

## Alert behavior

The code sets a red-zone threshold with:

```cpp
const float ALERT_DISTANCE_M = 1.5;
```

Any target closer than or equal to 1.5 meters is considered inside the alert area. When that happens:

- the UI shows a red alert banner
- the LED blinks red
- the target is highlighted in the radar display

Set `ALERT_DISTANCE_M` to `0` to disable the alert radius.

## LED behavior

The board’s RGB LED is used as a status indicator:

- Green: idle / system ready
- Blue: connecting to Wi-Fi
- Red blinking: alert active
- Off or steady green when no target is being tracked

## Software setup

### Arduino IDE / Arduino CLI

Open the project in the Arduino IDE or use `arduino-cli`.

Install these libraries using the library manager:

- Adafruit NeoPixel
- WebSockets by Markus Sattler (also known as arduinoWebSockets)

### Wi-Fi configuration

Edit the top of `ttkmmwave.ino` and replace these values:

```cpp
const char* WIFI_SSID = "<YOUR WIFI SSID>";
const char* WIFI_PASS = "<YOUR WIFI PASSWORD>";
```

If you do not want to use a local Wi-Fi network, the board will still fall back to its AP.

### Board settings

Use these settings when compiling:

- Board: `ESP32C3 Dev Module`
- Upload speed: `921600`
- CDC on boot: `Enabled`
- Flash mode: default for your board

The included `build.sh` script assumes `arduino-cli` is installed and uses the ESP32C3 profile.

## Build and upload

### Option 1: Use the shell script

```bash
./build.sh
```

This script:

- compiles the sketch
- detects connected USB devices
- lets you pick the serial port
- uploads the firmware to the board

### Option 2: Arduino IDE

- Open `ttkmmwave.ino`
- Select the correct board
- Select the correct COM/serial port
- Click Upload

## Serial monitor

To view the debug output from the ESP32:

```bash
./monitor.sh
```

This opens the serial monitor for the connected USB board so you can see connection status, Wi-Fi events, and radar health output.

## Running the project

1. Wire the sensor and ESP32 together.
2. Configure Wi-Fi SSID/password in the Arduino file.
3. Compile and upload the sketch.
4. Power the board.
5. Connect to the ESP32 over Wi-Fi or the fallback AP.
6. Open the IP in a browser.

The dashboard should display a radar view and live moving targets.

## Troubleshooting

### The board does not see the radar

- Check UART wiring
- Ensure the ground is connected
- Confirm the radar TX/RX direction
- Confirm the UART speed: `256000`

### No Wi-Fi connection

- Check SSID/password
- Verify the router is in range
- The board will fall back to the AP automatically

### No target data appears

- Check serial output in the monitor
- Make sure the radar is powered correctly
- Confirm the LD2450 is transmitting valid frames

### Browser page is blank or disconnected

- Make sure the board has booted and connected to Wi-Fi
- Check the board serial log for WebSocket or networking events
- Make sure the browser is loading the correct IP

## Notes

- The project is designed for interactive local monitoring and radar visualization.
- The default code is tuned for a live target display and short-range alerting.
- Several values are compile-time constants, so you can tune the radar behavior to your installation.

## File layout

```text
.
├── ttkmmwave.ino   # Main ESP32 firmware
├── example.go      # Go-based websocket/serial bridge example
├── build.sh        # compile + upload helper
├── monitor.sh      # serial monitor helper
├── README.md       # Project documentation
```

## Example Go bridge

The `example.go` file is a separate Go program that reads serial data from a USB device and serves a browser dashboard over WebSocket. It is a useful reference for how the radar stream is decoded and forwarded to a browser-based UI.

This is not strictly required for the main ESP32 firmware, but it shows the same radar data model in another environment.

## Summary

This project combines a HLK-LD2450 mmWave sensor with an ESP32 and a lightweight web interface. It decodes the radar data stream, builds a live target map, and exposes it over Wi-Fi so the sensor can be used as a compact real-time presence and movement monitor.
