# DJOneHub for Linux

DJOneHub on Linux runs as a user service and serves the web console at
`http://127.0.0.1:7575`. Call audio uses Quectel Voice over USB
(`AT+QPCMV=1,0`): the module streams 8 kHz PCM on its NMEA interface, and
DJOneHub bridges that stream to PipeWire through `pw-cat` (raw mode).

## Port layout (Quectel EC20/EC25/EG25, 2c7c:0125)

| Interface | Device      | Owner        | Purpose                  |
| --------- | ----------- | ------------ | ------------------------ |
| MI_00     | ttyUSB0     | —            | DM (diagnostics)         |
| MI_01     | ttyUSB1     | DJOneHub     | Voice over USB PCM       |
| MI_02     | ttyUSB2     | ModemManager | AT                       |
| MI_03     | ttyUSB3     | DJOneHub     | AT (SMS, calls, eSIM)    |
| MI_04     | cdc-wdm0    | ModemManager | QMI + mobile data        |

`70-djonehub.rules` sets `ID_MM_PORT_IGNORE` on MI_01 and MI_03, so
ModemManager releases them and mobile data keeps working. With the rule
installed, ModemManager no longer uses the GPS port.

The module must send URCs (incoming SMS `+CMTI` and `RING`) to MI_03:

```
AT+QURCCFG="urcport","usbmodem"
```

## Install / uninstall

```sh
sudo linux/install.sh <desktop-user>
sudo linux/uninstall.sh <desktop-user>
```

## Logs

```sh
journalctl --user -u djonehub -f
```

## Echo

Laptop speakers and microphone feed back into the call. Use a headset, or
load PipeWire's echo-cancel module. Then set `DJONEHUB_AUDIO_SINK` and
`DJONEHUB_AUDIO_SOURCE` in the unit, for example with
`systemctl --user edit djonehub`.
