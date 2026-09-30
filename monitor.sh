#!/bin/bash
# set -e

BOARD="esp32:esp32:esp32s3"
BAUDRATE=115200

# Kill any previous monitor processes
pkill -f "arduino-cli monitor" 2>/dev/null || true

# Get list of USB-connected boards
echo "Detecting connected boards..."
AVAILABLE_PORTS=$(arduino-cli board list | grep "(USB)" | awk '{print $1, $6}')
IFS=$'\n' read -rd '' -a PORT_LIST <<<"$AVAILABLE_PORTS"

# If no ports found, exit
if [ ${#PORT_LIST[@]} -eq 0 ]; then
    echo "❌ No connected USB boards detected. Please plug in your device."
    exit 1
fi

# Show menu
for i in "${!PORT_LIST[@]}"; do
    PORT=$(echo "${PORT_LIST[$i]}" | awk '{print $1}')
    BOARD_NAME=$(echo "${PORT_LIST[$i]}" | awk '{print $2}')
    echo "[$i] Port: $PORT | Board: $BOARD_NAME"
done

# Get user choice
read -p "Enter the number of the port to use: " SELECTED_INDEX

# Validate input
if ! [[ "$SELECTED_INDEX" =~ ^[0-9]+$ ]] || [ "$SELECTED_INDEX" -lt 0 ] || [ "$SELECTED_INDEX" -ge "${#PORT_LIST[@]}" ]; then
    echo "❌ Invalid selection."
    exit 1
fi

# Extract selected port
SELECTED_PORT=$(echo "${PORT_LIST[$SELECTED_INDEX]}" | awk '{print $1}')

# Launch monitor
echo "✅ Launching serial monitor on $SELECTED_PORT at $BAUDRATE baud..."
arduino-cli monitor -p "$SELECTED_PORT" --fqbn "$BOARD" --config baudrate=$BAUDRATE
