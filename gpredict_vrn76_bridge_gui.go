//go:build linux

package main

/*
#cgo pkg-config: Qt6Widgets
#cgo CXXFLAGS: -std=c++17
#cgo LDFLAGS: -lstdc++
#include <string.h>
#include "qt_gui_bridge.h"
*/
import "C"

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"unsafe"
)

// bridgeSnapshot is a thread-safe view of the bridge state consumed by the UI.
type bridgeSnapshot struct {
	GpredictConnected bool
	RadioConnected    bool
	Connecting        bool
	RXHz              uint32
	TXHz              uint32
	ConnectError      string
}

func connectionLEDText(name string, connected bool) string {
	state := "disconnected"
	if connected {
		state = "connected"
	}
	return fmt.Sprintf("● %s: %s", name, state)
}

func frequencyText(frequency uint32) string {
	if frequency == 0 {
		return "—"
	}
	return fmt.Sprintf("%.6f MHz", float64(frequency)/1_000_000)
}

// radioRouter keeps rigctld usable before the radio is connected: frequency
// updates are cached by FrequencyControl and applied after a successful connect.
type radioRouter struct {
	mu    sync.RWMutex
	radio *Radio
}

func (r *radioRouter) SendMessage(message []byte) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.radio == nil || !r.radio.IsConnected() {
		return nil
	}
	return r.radio.SendMessage(message)
}

func (r *radioRouter) setRadio(radio *Radio) {
	r.mu.Lock()
	old := r.radio
	r.radio = radio
	r.mu.Unlock()
	if old != nil && old != radio {
		_ = old.Close()
	}
}

func (r *radioRouter) clearRadio(radio *Radio) {
	r.mu.Lock()
	if r.radio == radio {
		r.radio = nil
	}
	r.mu.Unlock()
}

func (r *radioRouter) connected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.radio != nil && r.radio.IsConnected()
}

func (r *radioRouter) close() {
	r.mu.Lock()
	radio := r.radio
	r.radio = nil
	r.mu.Unlock()
	if radio != nil {
		_ = radio.Close()
	}
}

// bridgeController owns the rigctld listener and the user-triggered RFCOMM
// connection. The Qt thread only polls Snapshot; network work stays off-thread.
type bridgeController struct {
	address string
	host    string
	port    int
	channel int
	verbose bool

	router   *radioRouter
	control  *FrequencyControl
	protocol *RigctlProtocol
	listener net.Listener

	mu              sync.RWMutex
	gpredictClients int
	connecting      bool
	connectError    string
	closed          bool
}

func newBridgeController(address, host string, port int, rxHz, txHz uint32, channel int, verbose bool) (*bridgeController, error) {
	router := &radioRouter{}
	control := NewFrequencyControl(router, rxHz, txHz)
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen on %s:%d: %w", host, port, err)
	}
	controller := &bridgeController{
		address:  address,
		host:     host,
		port:     port,
		channel:  channel,
		verbose:  verbose,
		router:   router,
		control:  control,
		protocol: NewRigctlProtocol(control),
		listener: listener,
	}
	log.Printf("rigctld-compatible server listening on %s", listener.Addr())
	go controller.acceptClients()
	return controller, nil
}

func (b *bridgeController) acceptClients() {
	for {
		client, err := b.listener.Accept()
		if err != nil {
			b.mu.RLock()
			closed := b.closed
			b.mu.RUnlock()
			if !closed {
				log.Printf("rigctld accept failed: %v", err)
			}
			return
		}
		b.clientConnected()
		go func() {
			defer b.clientDisconnected()
			handleClient(client, b.protocol, b.control, b.verbose)
		}()
	}
}

func (b *bridgeController) clientConnected() {
	b.mu.Lock()
	b.gpredictClients++
	b.mu.Unlock()
}

func (b *bridgeController) clientDisconnected() {
	b.mu.Lock()
	if b.gpredictClients > 0 {
		b.gpredictClients--
	}
	b.mu.Unlock()
}

func (b *bridgeController) Snapshot() bridgeSnapshot {
	b.mu.RLock()
	snapshot := bridgeSnapshot{
		GpredictConnected: b.gpredictClients > 0,
		Connecting:        b.connecting,
		ConnectError:      b.connectError,
	}
	b.mu.RUnlock()
	snapshot.RadioConnected = b.router.connected()
	snapshot.RXHz = b.control.RXHz()
	snapshot.TXHz = b.control.TXHz()
	return snapshot
}

func (b *bridgeController) ConnectVRN76() {
	b.mu.Lock()
	if b.closed || b.connecting || b.router.connected() {
		b.mu.Unlock()
		return
	}
	b.connecting = true
	b.connectError = ""
	b.mu.Unlock()

	go func() {
		err := b.connectRadio()
		b.mu.Lock()
		b.connecting = false
		if err != nil {
			b.connectError = err.Error()
		}
		b.mu.Unlock()
	}()
}

func (b *bridgeController) connectRadio() error {
	radio, err := openRadioTransport(b.address, b.channel, b.verbose)
	if err != nil {
		return err
	}
	if err := sendRadioInitSequence(radio); err != nil {
		_ = radio.Close()
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = radio.Close()
		return fmt.Errorf("bridge is closing")
	}
	b.router.setRadio(radio)
	b.mu.Unlock()
	if err := b.control.ApplyCurrentFrequencies(); err != nil {
		b.router.clearRadio(radio)
		_ = radio.Close()
		return err
	}
	log.Printf("VR-N76 connected; current satellite frequencies applied")
	return nil
}

func (b *bridgeController) DisconnectVRN76() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.connecting {
		return
	}
	b.connectError = ""
	b.router.close()
}

func (b *bridgeController) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	listener := b.listener
	b.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	b.router.close()
}

var guiControllerMu sync.RWMutex
var guiController *bridgeController

//export goConnectVRN76
func goConnectVRN76() {
	guiControllerMu.RLock()
	controller := guiController
	guiControllerMu.RUnlock()
	if controller != nil {
		controller.ConnectVRN76()
	}
}

//export goDisconnectVRN76
func goDisconnectVRN76() {
	guiControllerMu.RLock()
	controller := guiController
	guiControllerMu.RUnlock()
	if controller != nil {
		controller.DisconnectVRN76()
	}
}

//export goBridgeSnapshot
func goBridgeSnapshot(gpredictConnected, radioConnected, connecting *C.int,
	rxHz, txHz *C.uint32_t, errorBuffer *C.char, errorBufferLength C.int) {
	guiControllerMu.RLock()
	controller := guiController
	guiControllerMu.RUnlock()
	if controller == nil {
		return
	}
	snapshot := controller.Snapshot()
	if gpredictConnected != nil {
		*gpredictConnected = cBool(snapshot.GpredictConnected)
	}
	if radioConnected != nil {
		*radioConnected = cBool(snapshot.RadioConnected)
	}
	if connecting != nil {
		*connecting = cBool(snapshot.Connecting)
	}
	if rxHz != nil {
		*rxHz = C.uint32_t(snapshot.RXHz)
	}
	if txHz != nil {
		*txHz = C.uint32_t(snapshot.TXHz)
	}
	writeSnapshotError(errorBuffer, errorBufferLength, snapshot.ConnectError)
}

func writeSnapshotError(errorBuffer *C.char, errorBufferLength C.int, connectError string) {
	if errorBuffer == nil || errorBufferLength <= 0 {
		return
	}
	C.memset(unsafe.Pointer(errorBuffer), 0, C.size_t(errorBufferLength))
	message := []byte(connectError)
	if len(message) >= int(errorBufferLength) {
		message = message[:int(errorBufferLength)-1]
	}
	if len(message) > 0 {
		C.memcpy(unsafe.Pointer(errorBuffer), unsafe.Pointer(&message[0]), C.size_t(len(message)))
	}
}

func cBool(value bool) C.int {
	if value {
		return 1
	}
	return 0
}

func runBridgeGUI(controller *bridgeController) {
	guiControllerMu.Lock()
	guiController = controller
	guiControllerMu.Unlock()
	C.qt_bridge_run()
	guiControllerMu.Lock()
	guiController = nil
	guiControllerMu.Unlock()
}
