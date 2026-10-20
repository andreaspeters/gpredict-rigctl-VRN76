package main

import (
	"net"
	"testing"
	"time"
)

func TestConnectionLEDTextReflectsConnectionState(t *testing.T) {
	tests := []struct {
		name      string
		connected bool
		want      string
	}{
		{name: "disconnected", connected: false, want: "● GPredict: disconnected"},
		{name: "connected", connected: true, want: "● GPredict: connected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := connectionLEDText("GPredict", test.connected); got != test.want {
				t.Fatalf("connectionLEDText() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFrequencyTextUsesMHzWithSixDecimalPlaces(t *testing.T) {
	tests := []struct {
		frequency uint32
		want      string
	}{
		{frequency: 437_800_000, want: "437.800000 MHz"},
		{frequency: 145_990_000, want: "145.990000 MHz"},
		{frequency: 0, want: "—"},
	}
	for _, test := range tests {
		if got := frequencyText(test.frequency); got != test.want {
			t.Errorf("frequencyText(%d) = %q, want %q", test.frequency, got, test.want)
		}
	}
}

func TestDisconnectVRN76ClosesRadioAndClearsConnectionState(t *testing.T) {
	radioSide, peer := net.Pipe()
	defer peer.Close()

	radio := NewRadio(radioSide, false)
	router := &radioRouter{radio: radio}
	controller := &bridgeController{
		router:  router,
		control: NewFrequencyControl(router, 437_800_000, 145_990_000),
	}

	if !controller.Snapshot().RadioConnected {
		t.Fatal("radio should start connected")
	}
	controller.DisconnectVRN76()
	if controller.Snapshot().RadioConnected {
		t.Fatal("radio should be disconnected after DisconnectVRN76")
	}
	select {
	case <-radio.done:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not close the radio connection")
	}
}
