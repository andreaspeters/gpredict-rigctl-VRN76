#ifndef GPREDICT_VRN76_QT_GUI_BRIDGE_H
#define GPREDICT_VRN76_QT_GUI_BRIDGE_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

void qt_bridge_run(void);
void goConnectVRN76(void);
void goDisconnectVRN76(void);
void goBridgeSnapshot(int *gpredict_connected, int *radio_connected,
                      int *connecting, uint32_t *rx_hz, uint32_t *tx_hz,
                      char *error_buffer, int error_buffer_length);

#ifdef __cplusplus
}
#endif

#endif
