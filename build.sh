#FQBN="esp32:esp32:esp32s3:CDCOnBoot=cdc,FlashMode=qio,FlashSize=16M,PartitionScheme=app3M_fat9M_16MB,PSRAM=opi,CPUFreq=240,DebugLevel=none,DFUOnBoot=default,EraseFlash=none,EventsCore=1,JTAGAdapter=default,LoopCore=1,MSCOnBoot=default,UploadMode=default,UploadSpeed=921600,USBMode=hwcdc,ZigbeeMode=default"
#FQBN="esp32:esp32:XIAO_ESP32C6:UploadSpeed=921600,CDCOnBoot=cdc,CPUFreq=160,FlashFreq=80,FlashMode=qio,FlashSize=4M,PartitionScheme=default,DebugLevel=none,EraseFlash=none,JTAGAdapter=default,ZigbeeMode=default"
FQBN="esp32:esp32:esp32c3:UploadSpeed=921600,CDCOnBoot=cdc,CPUFreq=160,FlashFreq=80,FlashMode=qio,FlashSize=4M,PartitionScheme=default,DebugLevel=none,EraseFlash=none,JTAGAdapter=default"  
arduino-cli compile --fqbn "${FQBN}" ./ttkmmwave.ino --build-path ~/build

if [ $? -eq 0 ]; then
    echo "Detecting connected boards..."
    AVAILABLE_PORTS=$(arduino-cli board list | grep "(USB)" | awk '{print $1, $6}')
    IFS=$'\n' read -rd '' -a PORT_LIST <<<"$AVAILABLE_PORTS"

    if [ ${#PORT_LIST[@]} -eq 0 ]; then
        echo "***** USB PORT NOT FOUND *****"
        exit 1
    fi

    for i in "${!PORT_LIST[@]}"; do
        PORT=$(echo "${PORT_LIST[$i]}" | awk '{print $1}')
        BOARD_NAME=$(echo "${PORT_LIST[$i]}" | awk '{print $2}')
        echo "[$i] Port: $PORT | Board: $BOARD_NAME"
    done

    read -p "Enter the number of the port to use: " SELECTED_INDEX
    if ! [[ "$SELECTED_INDEX" =~ ^[0-9]+$ ]] || [ "$SELECTED_INDEX" -lt 0 ] || [ "$SELECTED_INDEX" -ge "${#PORT_LIST[@]}" ]; then
        echo "❌ Invalid selection."
        exit 1
    fi

    SELECTED_PORT=$(echo "${PORT_LIST[$SELECTED_INDEX]}" | awk '{print $1}')
    arduino-cli upload -p "${SELECTED_PORT}" --fqbn "${FQBN}" ./ttkmmwave --input-dir ~/build
fi